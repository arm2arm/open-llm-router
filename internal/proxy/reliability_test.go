package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/pool"
)

func TestEarlyHeadersFirstByteBudget(t *testing.T) {
	for _, protocol := range []string{config.ProtocolOpenAI, config.ProtocolAnthropic} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/timeout=%v", protocol, timeout), func(t *testing.T) {
				var calls atomic.Int32
				serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/health" {
						return
					}
					calls.Add(1)
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					http.NewResponseController(w).Flush()
					if timeout {
						<-r.Context().Done()
						return
					}
					select {
					case <-time.After(1300 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					if protocol == config.ProtocolAnthropic {
						io.WriteString(w, "event: message_stop\ndata: {}\n\n")
					} else {
						io.WriteString(w, "data: [DONE]\n\n")
					}
				})
				backend, second := httptest.NewServer(serve), httptest.NewServer(serve)
				defer backend.Close()
				defer second.Close()
				cfg := testConfig(backend.URL, second.URL)
				m := cfg.Models["chat-model"]
				m.TimeoutS, m.IdleTimeoutS, m.QueueTimeoutS = 3, 1, 2
				if timeout {
					m.TimeoutS, m.QueueTimeoutS = 1, 1
				}
				cfg.Models["chat-model"] = m
				h, router := configured(t, cfg)
				path := "/v1/chat/completions"
				if protocol == config.ProtocolAnthropic {
					path = "/v1/messages"
				}
				resp, body := send(t, router.URL, path, `{"model":"chat-model","stream":true}`, nil)
				want := 200
				if timeout {
					want = 504
					if !strings.Contains(string(body), "upstream response timed out") || !strings.Contains(string(body), "timeout_error") {
						t.Fatalf("timeout envelope: %s", body)
					}
				}
				if resp.StatusCode != want || calls.Load() != 1 || !h.Pools.Ready() || h.Pools.Pools["chat-model"].Queued() != 0 {
					t.Fatalf("status=%d calls=%d states=%+v", resp.StatusCode, calls.Load(), h.Pools.Pools["chat-model"].Snapshot())
				}
				for _, state := range h.Pools.Pools["chat-model"].Snapshot() {
					if !state.Healthy {
						t.Fatalf("prefill cooled a replica: %+v", state)
					}
				}
			})
		}
	}
}

func TestSingleReplicaFailureDoesNotQueueRetry(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer backend.Close()
	cfg := testConfig(backend.URL)
	m := cfg.Models["chat-model"]
	m.QueueTimeoutS = 5
	cfg.Models["chat-model"] = m
	h, router := configured(t, cfg)
	start := time.Now()
	resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model"}`, nil)
	if resp.StatusCode != 502 || calls.Load() != 1 || time.Since(start) > 2*time.Second || h.Pools.Pools["chat-model"].Queued() != 0 {
		t.Fatalf("failed request status=%d calls=%d elapsed=%s", resp.StatusCode, calls.Load(), time.Since(start))
	}
}

func TestStreamTerminationAgreesWithUsage(t *testing.T) {
	for _, tc := range []struct {
		name, path, event, ending, status string
	}{
		{"openai/truncated", "/v1/chat/completions", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n", "", "aborted"},
		{"openai/complete", "/v1/chat/completions", "data: {}\n\n", "data: [DONE]\n\n", "200"},
		{"openai/error", "/v1/chat/completions", "data: {\"error\":{\"message\":\"failed\"}}\n\n", "", "error"},
		{"anthropic/truncated", "/v1/messages", "event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n", "", "aborted"},
		{"anthropic/complete", "/v1/messages", "event: message_delta\ndata: {}\n\n", "event: message_stop\ndata: {}\n\n", "200"},
		{"anthropic/error", "/v1/messages", "event: error\ndata: {\"error\":{\"message\":\"failed\"}}\n\n", "", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.event+tc.ending)
			}))
			defer backend.Close()
			cfg := testConfig(backend.URL)
			cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			records := make(recordChannel, 1)
			_, router := accountingRouter(t, cfg, records)
			resp, err := http.Post(router.URL+tc.path, "application/json", strings.NewReader(`{"model":"chat-model","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(body) != tc.event+tc.ending || (readErr != nil) != (tc.status == "aborted") {
				t.Fatalf("status=%d body=%q read error=%v", resp.StatusCode, body, readErr)
			}
			if r := records.next(t); r.Status != tc.status {
				t.Fatalf("usage status=%s want %s", r.Status, tc.status)
			}
		})
	}
}

func TestInferenceStatusDoesNotChangeProbeHealth(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			calls.Add(1)
			http.Error(w, "model runner unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer backend.Close()
	h, router := setup(t, backend.URL)
	for range 2 {
		resp, body := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model"}`, nil)
		if resp.StatusCode != 503 || !strings.Contains(string(body), "model runner unavailable") || !h.Pools.Ready() {
			t.Fatal("inference status must pass through without cooldown or replay")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("inference response replayed", calls.Load())
	}
}

func TestOverlongSessionIsRejectedBeforeBackend(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			calls.Add(1)
		}
	}))
	defer backend.Close()
	_, router := setup(t, backend.URL)
	resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"chat-model"}`, http.Header{"X-Session-Id": {strings.Repeat("x", pool.MaxSessionIDBytes+1)}})
	if resp.StatusCode != 400 || calls.Load() != 0 {
		t.Fatal("overlong session reached backend", resp.StatusCode, calls.Load())
	}
}
