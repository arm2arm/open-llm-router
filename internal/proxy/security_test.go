package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/server"
)

func TestParseRequestRefusesAmbiguousRoutingFields(t *testing.T) {
	for body, want := range map[string]requestMeta{
		`{"model":"a"}`:                                   {Model: "a"},
		`{"model":"a","stream":true}`:                     {Model: "a", Stream: true},
		`{"model":"a","stream":null}`:                     {Model: "a"},
		`{"messages":[{"Model":"x"}],"model":"a"}`:        {Model: "a"},
		` {"model" : "a" , "n" : 12345678901234567890 } `: {Model: "a"},
	} {
		got, err := parseRequest([]byte(body))
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", body, got, err)
		}
	}
	for _, body := range []string{
		`{"MODEL":"a"}`, `{"Model":"a"}`, `{"model":"a","MODEL":"b"}`, `{"model":"a","model":"b"}`,
		`{"model":"a","Stream":true}`, `{"model":"a","stream":false,"stream":true}`,
	} {
		if _, err := parseRequest([]byte(body)); err != errAmbiguousField {
			t.Errorf("%s: err = %v, want ambiguous field", body, err)
		}
	}
	for _, body := range []string{``, `[]`, `"x"`, `{"model":1}`, `{"model":"a","stream":"yes"}`, `{"model":"a"} {}`, `{"model":"a"`, `{"model":"a",}`} {
		if _, err := parseRequest([]byte(body)); err == nil {
			t.Errorf("%s: accepted", body)
		}
	}
}

// One backend serving several models (Ollama, vLLM LoRA adapters or several
// served names) must never receive a model that the caller is not allowed.
func TestAllowListCannotBeBypassedByKeySpelling(t *testing.T) {
	var reached atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			reached.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer backend.Close()
	replica := []config.Replica{{URL: backend.URL, Weight: 1, MaxInflight: 4}}
	cfg := &config.Config{Models: map[string]config.Model{
		"allowed": {Protocols: []string{"openai", "anthropic"}, TimeoutS: 3, IdleTimeoutS: 2, Pool: replica},
		"secret":  {Protocols: []string{"openai", "anthropic"}, TimeoutS: 3, IdleTimeoutS: 2, Pool: replica},
	}}
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"allowed"}}}
	_, router := configuredForGroups(t, cfg, []string{"staff"})
	for _, body := range []string{
		`{"model":"secret","MODEL":"allowed"}`,
		`{"Model":"secret","model":"allowed"}`,
		`{"model":"secret","model":"allowed"}`,
		`{"model":"allowed","Stream":true}`,
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
			resp, data := send(t, router.URL, path, body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %s: %d %s", path, body, resp.StatusCode, data)
			}
		}
	}
	if reached.Load() != 0 {
		t.Fatal("an ambiguous request reached the backend")
	}
	if resp, data := send(t, router.URL, "/v1/chat/completions", `{"model":"allowed"}`, nil); resp.StatusCode != 200 {
		t.Fatalf("allowed model: %d %s", resp.StatusCode, data)
	}
}

func TestBackendCannotSetCookiesOnRouterOrigin(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "olr_session", Value: "planted", Path: "/portal"})
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	_, router := setup(t, backend.URL)
	resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model"}`, nil)
	if resp.StatusCode != 200 || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("status %d, Set-Cookie %q", resp.StatusCode, resp.Header.Values("Set-Cookie"))
	}
}

func TestSlowRequestBodyTimesOutButLongResponsesStream(t *testing.T) {
	previous := server.RequestBodyTimeout
	server.RequestBodyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { server.RequestBodyTimeout = previous })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// Stream for longer than the body deadline.
		for range 8 {
			io.WriteString(w, " ")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		io.WriteString(w, `{}`)
	}))
	defer backend.Close()
	_, router := setup(t, backend.URL)

	// A body that never completes is cut off with 408 after the deadline.
	conn, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"model\":")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	started := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout || time.Since(started) > 3*time.Second {
		t.Fatalf("slow body: status %d after %s", resp.StatusCode, time.Since(started))
	}

	// A complete body clears the deadline; the response may outlast it.
	resp2, data := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model"}`, nil)
	if resp2.StatusCode != 200 || !strings.HasSuffix(string(data), "{}") {
		t.Fatalf("long response: %d %q", resp2.StatusCode, data)
	}
}

func TestUnreadBodyOnOtherRoutesIsBounded(t *testing.T) {
	previous := server.RequestBodyTimeout
	server.RequestBodyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { server.RequestBodyTimeout = previous })
	_, router := setup(t, "http://127.0.0.1:1")
	conn, err := net.Dial("tcp", strings.TrimPrefix(router.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /unknown HTTP/1.1\r\nHost: x\r\nContent-Length: 100000\r\n\r\nx")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// The server must close the connection instead of waiting for the body.
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("connection stayed open: %v", err)
	}
}
