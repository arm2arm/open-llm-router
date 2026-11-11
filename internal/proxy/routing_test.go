package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/server"
)

// blocking is a backend that holds each POST until released and records the
// priority field it received.
type blocking struct {
	mu         sync.Mutex
	priorities []any
	release    chan struct{}
	started    chan struct{}
}

func (b *blocking) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	b.mu.Lock()
	b.priorities = append(b.priorities, body["priority"])
	b.mu.Unlock()
	if b.started != nil {
		b.started <- struct{}{}
	}
	if b.release != nil {
		<-b.release
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{}`)
}

func routed(t *testing.T, model config.Model, class string, backends ...string) string {
	t.Helper()
	cfg := &config.Config{Models: map[string]config.Model{}, Groups: map[string]config.Group{"g": {Models: []string{"*"}, Priority: 1}}}
	for _, u := range backends {
		model.Pool = append(model.Pool, config.Replica{URL: u, Weight: 1, MaxInflight: 1})
	}
	model.Protocols, model.TimeoutS, model.IdleTimeoutS = []string{"openai", "anthropic"}, 5, 5
	cfg.Models["m"] = model
	h := New(cfg)
	h.Pools.Check(t.Context())
	s := server.New(slog.New(slog.DiscardHandler))
	s.SetAPIMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), auth.Principal{Groups: []string{"g"}, KeyClass: class})))
		})
	})
	h.Register(s.Handle)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); h.Close() })
	return ts.URL
}

func TestQueueThroughTheProxy(t *testing.T) {
	b := &blocking{release: make(chan struct{}), started: make(chan struct{}, 4)}
	backend := httptest.NewServer(b)
	defer backend.Close()
	router := routed(t, config.Model{QueueTimeoutS: 2}, "interactive", backend.URL)
	statuses := make(chan int, 2)
	for range 2 {
		go func() { statuses <- post(router) }()
	}
	<-b.started
	// The second call waits in the router's queue instead of failing.
	time.Sleep(100 * time.Millisecond)
	b.release <- struct{}{}
	<-b.started
	b.release <- struct{}{}
	if a, c := <-statuses, <-statuses; a != 200 || c != 200 {
		t.Fatal("short overload was not absorbed", a, c)
	}
}

func TestQueueTimeoutAnswers503(t *testing.T) {
	b := &blocking{release: make(chan struct{}), started: make(chan struct{}, 4)}
	backend := httptest.NewServer(b)
	defer backend.Close()
	defer close(b.release)
	router := routed(t, config.Model{QueueTimeoutS: 1}, "interactive", backend.URL)
	go post(router)
	<-b.started
	started := time.Now()
	resp, _ := send(t, router, "/v1/chat/completions", `{"model":"m"}`, nil)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || time.Since(started) < time.Second {
		t.Fatal("long overload", resp.StatusCode, time.Since(started))
	}
}

func TestPriorityIsSetByTheRouter(t *testing.T) {
	b := &blocking{}
	backend := httptest.NewServer(b)
	defer backend.Close()
	priorities := &config.Priorities{Interactive: 0, Batch: 10}
	for class, want := range map[string]float64{"interactive": 1, "batch": 11} {
		router := routed(t, config.Model{Priority: priorities}, class, backend.URL)
		// A caller cannot choose its own priority.
		resp, body := send(t, router, "/v1/messages", `{"model":"m","priority":-100,"max_tokens":1}`, nil)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode, string(body))
		}
		b.mu.Lock()
		got := b.priorities[len(b.priorities)-1]
		b.mu.Unlock()
		if got != want {
			t.Fatalf("%s: priority %v, want %v", class, got, want)
		}
	}
	// Without a configured priority the field passes through untouched.
	router := routed(t, config.Model{}, "batch", backend.URL)
	send(t, router, "/v1/chat/completions", `{"model":"m","priority":3}`, nil)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.priorities[len(b.priorities)-1] != float64(3) {
		t.Fatal("unconfigured priority was changed", b.priorities)
	}
}

func TestSessionAffinityThroughTheProxy(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	var urls []string
	for range 4 {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				mu.Lock()
				hits[r.Host]++
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}))
		defer backend.Close()
		urls = append(urls, backend.URL)
	}
	router := routed(t, config.Model{SessionAffinity: true}, "interactive", urls...)
	for range 20 {
		send(t, router, "/v1/chat/completions", `{"model":"m"}`, http.Header{"X-Session-Id": {"chat-42"}})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 {
		t.Fatalf("one session spread over replicas: %v", hits)
	}
}

// post may run in its own goroutine: it reports failures as status 0.
func post(router string) int {
	resp, err := http.Post(router+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		return 0
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}
