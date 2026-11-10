package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/server"
)

func setup(t *testing.T, backends ...string) (*Handler, *httptest.Server) {
	t.Helper()
	return configured(t, testConfig(backends...))
}

func testConfig(backends ...string) *config.Config {
	cfg := &config.Config{Models: map[string]config.Model{"chat-model": {Protocols: []string{"openai", "anthropic"}, TimeoutS: 3, IdleTimeoutS: 2}}, Aliases: map[string]string{"alias": "chat-model"}}
	m := cfg.Models["chat-model"]
	for _, u := range backends {
		m.Pool = append(m.Pool, config.Replica{URL: u, Weight: 1, MaxInflight: 64})
	}
	cfg.Models["chat-model"] = m
	return cfg
}

func configured(t *testing.T, cfg *config.Config) (*Handler, *httptest.Server) {
	t.Helper()
	h := New(cfg)
	h.Pools.Check(t.Context())
	s := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetReadiness(h.Pools.Ready)
	h.Register(s.Handle)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); h.Close() })
	return h, ts
}

func send(t *testing.T, base, path, body string, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition not satisfied within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBothProtocolsAgainstMock(t *testing.T) {
	mock := mockllm.New(mockllm.Options{Tokens: 4, TokenDelay: time.Millisecond})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	_, router := setup(t, backend.URL)
	for _, tc := range []struct{ path, extra, contains string }{
		{"/v1/chat/completions", `"messages":[{"role":"user","content":"hello"}]`, `"chat.completion"`},
		{"/v1/chat/completions", `"messages":[],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]`, `tool_calls`},
		{"/v1/chat/completions", `"messages":[],"stream":true`, `data: [DONE]`},
		{"/v1/chat/completions", `"messages":[],"stream":true,"tools":[{"type":"function","function":{"name":"weather"}}]`, `tool_calls`},
		{"/v1/completions", `"prompt":"hello"`, `text_completion`},
		{"/v1/completions", `"prompt":"hello","stream":true`, `data: [DONE]`},
		{"/v1/embeddings", `"input":["hello","there"]`, `embedding`},
		{"/v1/messages", `"messages":[{"role":"user","content":"hello"}],"max_tokens":8`, `end_turn`},
		{"/v1/messages", `"messages":[],"max_tokens":8,"stream":true`, `event: message_stop`},
		{"/v1/messages", `"messages":[],"max_tokens":8,"tools":[{"name":"weather","input_schema":{"type":"object"}}]`, `tool_use`},
		{"/v1/messages", `"messages":[],"max_tokens":8,"stream":true,"tools":[{"name":"weather","input_schema":{"type":"object"}}]`, `input_json_delta`},
		{"/v1/messages/count_tokens", `"messages":[{"role":"user","content":"hello"}]`, `input_tokens`},
	} {
		t.Run(tc.path+tc.contains, func(t *testing.T) {
			resp, b := send(t, router.URL, tc.path, `{"model":"chat-model",`+tc.extra+`}`, nil)
			if resp.StatusCode != 200 || !bytes.Contains(b, []byte(tc.contains)) {
				t.Fatalf("%d %s", resp.StatusCode, b)
			}
			if resp.Header.Get("X-Request-Id") == "" || resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache" {
				t.Fatal(resp.Header)
			}
		})
	}
}

func TestPassthroughHeadersAliasesAndListing(t *testing.T) {
	mock := mockllm.New(mockllm.Options{})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	cfg := testConfig(backend.URL)
	model := cfg.Models["chat-model"]
	model.Pool[0].BearerToken = "backend-token"
	_, router := configured(t, cfg)
	original := "{\n  \"model\":\"chat-model\", \"messages\":[], \"unknown\":{\"big\":123456789012345678901234567890,\"array\":[1,true]}\n}"
	headers := http.Header{"Authorization": {"Bearer client-secret"}, "X-Api-Key": {"client-key"}, "Cookie": {"session=secret"}, "Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"tools-beta"}, "Connection": {"X-Remove"}, "X-Remove": {"gone"}, "Proxy-Authorization": {"secret"}}
	for _, alias := range []bool{false, true} {
		body := original
		if alias {
			body = strings.Replace(body, "chat-model", "alias", 1)
		}
		resp, _ := send(t, router.URL, "/v1/chat/completions?raw=a%2Fb&raw=two", body, headers)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		last := mock.Stats().Last
		if !alias && string(last.Body) != original {
			t.Fatalf("body changed: %s", last.Body)
		}
		if alias && (!bytes.Contains(last.Body, []byte(`"model":"chat-model"`)) || !bytes.Contains(last.Body, []byte(`123456789012345678901234567890`))) {
			t.Fatal(string(last.Body))
		}
		if last.Path != "/v1/chat/completions?raw=a%2Fb&raw=two" {
			t.Fatal(last.Path)
		}
		if last.Header.Get("Authorization") != "Bearer backend-token" || last.Header.Get("X-Api-Key") != "" || last.Header.Get("Cookie") != "" || last.Header.Get("X-Remove") != "" || last.Header.Get("Proxy-Authorization") != "" {
			t.Fatal("credential or hop header forwarded")
		}
		for key, want := range map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "tools-beta", "Accept-Encoding": "identity", "X-Request-Id": resp.Header.Get("X-Request-Id")} {
			if last.Header.Get(key) != want {
				t.Errorf("%s = %q", key, last.Header.Get(key))
			}
		}
	}
	resp, err := http.Get(router.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Object string
		Data   []struct{ ID string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 2 || list.Data[0].ID != "alias" || list.Data[1].ID != "chat-model" {
		t.Fatal(list)
	}
}

func TestUpstreamModelAndProtocolErrors(t *testing.T) {
	mock := mockllm.New(mockllm.Options{Models: []string{"served-name"}})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	cfg := &config.Config{Models: map[string]config.Model{"public-name": {Protocols: []string{"openai"}, UpstreamModel: "served-name", TimeoutS: 3, IdleTimeoutS: 2, Pool: []config.Replica{{URL: backend.URL, Weight: 1, MaxInflight: 4}}}}, Aliases: map[string]string{"alias": "public-name"}}
	_, router := configured(t, cfg)
	for _, name := range []string{"public-name", "alias"} {
		resp, body := send(t, router.URL, "/v1/chat/completions", `{"model":"`+name+`","messages":[]}`, nil)
		if resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"model":"served-name"`)) {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	}
	for _, tc := range []struct {
		path, body string
		status     int
		anthropic  bool
	}{
		{"/v1/messages", `{"model":"alias"}`, 404, true},
		{"/v1/messages", `{"model":"missing"}`, 404, true},
		{"/v1/chat/completions", `{"model":"missing"}`, 404, false},
		{"/v1/chat/completions", `{"model":`, 400, false},
		{"/v1/messages", `{"stream":"yes","model":"alias"}`, 400, true},
		{"/v1/chat/completions", `null`, 400, false},
		{"/v1/messages", `{"model":"alias","extra":"` + strings.Repeat("x", MaxBody) + `"}`, 413, true},
	} {
		resp, body := send(t, router.URL, tc.path, tc.body, nil)
		var shape map[string]json.RawMessage
		if json.Unmarshal(body, &shape) != nil || shape["error"] == nil || (string(shape["type"]) == `"error"`) != tc.anthropic || resp.StatusCode != tc.status {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	}
}

func TestIncrementalStreamingAnd100Disconnects(t *testing.T) {
	mock := mockllm.New(mockllm.Options{Tokens: 6, TokenDelay: 50 * time.Millisecond})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	h, router := setup(t, backend.URL)
	clientTransport := http.DefaultTransport.(*http.Transport).Clone()
	defer clientTransport.CloseIdleConnections()
	client := &http.Client{Transport: clientTransport}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		start := time.Now()
		resp, err := client.Post(router.URL+path, "application/json", strings.NewReader(`{"model":"chat-model","messages":[],"max_tokens":6,"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(resp.Body)
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		first := time.Since(start)
		_, err = io.Copy(io.Discard, reader)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		total := time.Since(start)
		if first > 150*time.Millisecond || total < 250*time.Millisecond {
			t.Fatalf("buffered stream: first=%s total=%s", first, total)
		}
	}
	clientTransport.CloseIdleConnections()
	h.Close()
	eventually(t, func() bool { return mock.Stats().Inflight == 0 })
	baseline := runtime.NumGoroutine()
	for i := 0; i < 100; i++ {
		path := "/v1/chat/completions"
		if i%2 == 1 {
			path = "/v1/messages"
		}
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, "POST", router.URL+path, strings.NewReader(`{"model":"chat-model","messages":[],"max_tokens":100,"stream":true}`))
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, err = bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		resp.Body.Close()
	}
	eventually(t, func() bool { return mock.Stats().StreamsAborted == 100 && mock.Stats().Inflight == 0 })
	clientTransport.CloseIdleConnections()
	h.Close()
	eventually(t, func() bool {
		for _, s := range h.Pools.Pools["chat-model"].Snapshot() {
			if s.Inflight != 0 {
				return false
			}
		}
		return runtime.NumGoroutine() <= baseline+8
	})
}

func TestUpstreamResponsesUnchanged(t *testing.T) {
	for _, status := range []int{200, 307, 400, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			payload := "exact bytes\n\x00unknown-event: preserved\n\n"
			var requests atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				requests.Add(1)
				w.Header().Set("Connection", "X-Hop")
				w.Header().Set("X-Hop", "remove")
				w.Header().Set("Location", "http://invalid.invalid/")
				w.Header().Set("X-Custom", "kept")
				w.Header().Set("X-Request-Id", "untrusted")
				w.WriteHeader(status)
				io.WriteString(w, payload)
			}))
			defer backend.Close()
			_, router := setup(t, backend.URL)
			req, _ := http.NewRequest("POST", router.URL+"/v1/messages", strings.NewReader(`{"model":"alias"}`))
			client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != status || string(body) != payload || requests.Load() != 1 || resp.Header.Get("X-Hop") != "" || resp.Header.Get("X-Custom") != "kept" || resp.Header.Get("X-Request-Id") == "untrusted" {
				t.Fatalf("%d %q %v", resp.StatusCode, body, err)
			}
		})
	}
}

func TestPassiveRetryAndNoRetryAfterBytes(t *testing.T) {
	for _, mid := range []bool{false, true} {
		t.Run(map[bool]string{false: "before bytes", true: "after bytes"}[mid], func(t *testing.T) {
			var calls [2]atomic.Int32
			var backends []*httptest.Server
			for i := range 2 {
				b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/health" {
						return
					}
					calls[i].Add(1)
					if i == 0 {
						if mid {
							io.WriteString(w, "partial\n")
							http.NewResponseController(w).Flush()
						}
						panic(http.ErrAbortHandler)
					}
					io.WriteString(w, "complete\n")
				}))
				defer b.Close()
				backends = append(backends, b)
			}
			h, router := setup(t, backends[0].URL, backends[1].URL)
			// A reserved slot forces the first selection onto the failing replica.
			p := h.Pools.Pools["chat-model"]
			l := p.Acquire(nil)
			// Repeat selection until the held lease belongs to backend 1.
			for l.Replica.URL != backends[1].URL {
				l.Release()
				l = p.Acquire(nil)
			}
			defer l.Release()
			resp, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"chat-model"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if mid {
				if readErr == nil || string(body) != "partial\n" || calls[1].Load() != 0 {
					t.Fatalf("response %q error %v calls %d", body, readErr, calls[1].Load())
				}
			} else {
				if readErr != nil || string(body) != "complete\n" || calls[0].Load() != 1 || calls[1].Load() != 1 {
					t.Fatalf("response %q error %v", body, readErr)
				}
			}
		})
	}
}

func TestSlowReplicaReceivesLessAndCapsHold(t *testing.T) {
	var mocks []*mockllm.Server
	var urls []string
	for _, delay := range []time.Duration{time.Millisecond, 3 * time.Millisecond} {
		m := mockllm.New(mockllm.Options{Tokens: 6, TokenDelay: delay})
		b := httptest.NewServer(m)
		defer b.Close()
		mocks = append(mocks, m)
		urls = append(urls, b.URL)
	}
	_, router := setup(t, urls...)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			for range 20 {
				resp, body := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model","messages":[]}`, nil)
				if resp.StatusCode != 200 {
					t.Errorf("%d %s", resp.StatusCode, body)
				}
			}
		})
	}
	wg.Wait()
	slow := mocks[1].Stats().Requests
	if slow > 96 {
		t.Fatalf("3x slower backend received %d/240 (>40%%)", slow)
	}
	// Separate router with a small cap under 200 simultaneous requests.
	cfg := &config.Config{Models: map[string]config.Model{"chat-model": {Protocols: []string{"openai"}, TimeoutS: 3, IdleTimeoutS: 2, Pool: []config.Replica{{URL: urls[0], Weight: 1, MaxInflight: 3}, {URL: urls[1], Weight: 1, MaxInflight: 3}}}}}
	h, capped := configured(t, cfg)
	for _, m := range mocks {
		m.Reset()
	}
	for range 200 {
		wg.Go(func() {
			resp, _ := send(t, capped.URL, "/v1/chat/completions", `{"model":"chat-model","messages":[]}`, nil)
			if resp.StatusCode != 200 && (resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "") {
				t.Errorf("unexpected status %d", resp.StatusCode)
			}
		})
	}
	wg.Wait()
	for _, m := range mocks {
		if s := m.Stats(); s.PeakInflight > 3 || s.Inflight != 0 {
			t.Fatal(s)
		}
	}
	for _, s := range h.Pools.Pools["chat-model"].Snapshot() {
		if s.Inflight != 0 {
			t.Fatal(s)
		}
	}
}

func TestHeaderAndIdleTimeouts(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "idle"}[idle], func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				io.Copy(io.Discard, r.Body)
				if idle {
					io.WriteString(w, "first\n")
					http.NewResponseController(w).Flush()
				}
				<-r.Context().Done()
			}))
			defer backend.Close()
			cfg := &config.Config{Models: map[string]config.Model{"chat-model": {Protocols: []string{"openai"}, TimeoutS: 1, IdleTimeoutS: 1, Pool: []config.Replica{{URL: backend.URL, Weight: 1, MaxInflight: 4}}}}}
			h, router := configured(t, cfg)
			start := time.Now()
			resp, err := http.Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"chat-model"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 2500*time.Millisecond {
				t.Fatal(elapsed)
			}
			if idle {
				if readErr == nil || string(body) != "first\n" {
					t.Fatalf("%q %v", body, readErr)
				}
			} else {
				if resp.StatusCode != 503 || h.Pools.Ready() {
					t.Fatalf("status %d ready %v", resp.StatusCode, h.Pools.Ready())
				}
			}
			eventually(t, func() bool { return h.Pools.Pools["chat-model"].Snapshot()[0].Inflight == 0 })
		})
	}
}

func TestFailureReadinessAndRecovery(t *testing.T) {
	first := mockllm.New(mockllm.Options{Tokens: 2, TokenDelay: time.Millisecond})
	second := mockllm.New(mockllm.Options{Tokens: 2, TokenDelay: time.Millisecond})
	a := httptest.NewServer(first)
	defer a.Close()
	b := httptest.NewServer(second)
	defer b.Close()
	h, router := setup(t, a.URL, b.URL)
	// Probes remove a replica after two failures; a healthy model remains ready.
	first.SetOptions(mockllm.Options{Fail: mockllm.FailUnhealthy})
	h.Pools.Check(t.Context())
	h.Pools.Check(t.Context())
	for range 10 {
		resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model","messages":[]}`, nil)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	if first.Stats().Requests != 0 || second.Stats().Requests != 10 {
		t.Fatal("unhealthy replica received traffic")
	}
	resp, err := http.Get(router.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	second.SetOptions(mockllm.Options{Fail: mockllm.FailUnhealthy})
	h.Pools.Check(t.Context())
	h.Pools.Check(t.Context())
	resp, err = http.Get(router.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal(resp.StatusCode)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		resp, body := send(t, router.URL, path, `{"model":"chat-model"}`, nil)
		if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" || !bytes.Contains(body, []byte(`"error"`)) {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
	}
	// The real periodic loop restores a recovered backend in at most one interval.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { h.Pools.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	first.SetOptions(mockllm.Options{Tokens: 2})
	deadline := time.Now().Add(9 * time.Second)
	for !h.Pools.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("replica not recovered within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, _ = send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model","messages":[]}`, nil)
	if resp.StatusCode != 200 || first.Stats().Requests != 1 {
		t.Fatal("recovered replica did not receive traffic")
	}
}

func TestStoppedReplicaFailsOver(t *testing.T) {
	a := httptest.NewServer(mockllm.New(mockllm.Options{}))
	defer a.Close()
	good := mockllm.New(mockllm.Options{})
	b := httptest.NewServer(good)
	defer b.Close()
	h, router := setup(t, a.URL, b.URL)
	a.Close()
	for range 20 {
		resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model","messages":[]}`, nil)
		if resp.StatusCode != 200 {
			t.Fatal("stopped replica caused a client error", resp.StatusCode)
		}
	}
	if good.Stats().Requests != 20 {
		t.Fatal(good.Stats().Requests)
	}
	for _, s := range h.Pools.Pools["chat-model"].Snapshot() {
		if s.Inflight != 0 {
			t.Fatal(s)
		}
	}
}
