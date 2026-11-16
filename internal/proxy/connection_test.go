package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func TestConnectionNativeAPIsAndAccounting(t *testing.T) {
	for _, api := range []string{"chat", "responses", "messages"} {
		t.Run(api, func(t *testing.T) {
			mock := mockllm.New(mockllm.Options{Tokens: 3, Models: []string{"native-model"}})
			backend := httptest.NewServer(mock)
			defer backend.Close()
			cfg := testConfig(backend.URL)
			cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			model := cfg.Models["chat-model"]
			model.UpstreamModel = "native-model"
			model.Pool[0].Backend, model.Pool[0].BearerToken = "gpu", "synthetic-backend-key"
			cfg.Models["chat-model"] = model
			cfg.Backends = map[string]config.BackendPolicy{"gpu": {Groups: []string{"staff"}}}
			records := make(recordChannel, 2)
			h, _ := accountingRouter(t, cfg, records)
			result := h.TestConnection(t.Context(), auth.Principal{UserID: "portal-user", Groups: []string{"staff"}}, ConnectionTest{Model: "chat-model", Backend: "gpu", API: api})
			if !result.OK || result.HTTPStatus != 200 || result.InputTokens != 4 || result.OutputTokens != 3 || result.RequestID == "" {
				t.Fatalf("result = %+v", result)
			}
			record := records.next(t)
			if record.UserID != "portal-user" || record.KeyID != "" || record.RequestID != result.RequestID || record.OutputTokens != 3 || record.IsStream {
				t.Fatalf("usage = %+v", record)
			}
			last := mock.Stats().Last
			if last.Header.Get("Authorization") != "Bearer synthetic-backend-key" || last.Header.Get("Cookie") != "" || last.Header.Get("X-Api-Key") != "" {
				t.Fatal("credential isolation", last.Header)
			}
			var body map[string]json.RawMessage
			_ = json.Unmarshal(last.Body, &body)
			limit := "max_tokens"
			if api == "responses" {
				limit = "max_output_tokens"
				if string(body["store"]) != "false" || last.Path != "/v1/responses" {
					t.Fatal("native stateless Responses", string(last.Body))
				}
			}
			if string(body[limit]) != "16" || string(body["model"]) != `"native-model"` || string(body["stream"]) != "false" {
				t.Fatal("unbounded or unrewritten diagnostic", string(last.Body))
			}
		})
	}
}

func TestConnectionPinsBackendAndDoesNotExposeErrors(t *testing.T) {
	var failed, other atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = io.WriteString(w, `{"data":[{"id":"model"}]}`)
			return
		}
		failed.Add(1)
		http.Error(w, "private-upstream-error-with-secret", http.StatusServiceUnavailable)
	}))
	defer backend.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			other.Add(1)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
	}))
	defer good.Close()
	cfg := testConfig(backend.URL, good.URL)
	model := cfg.Models["chat-model"]
	model.Pool[0].Backend, model.Pool[1].Backend = "target", "other"
	cfg.Models["chat-model"] = model
	h, _ := configured(t, cfg)
	principal := auth.Principal{UserID: "admin", ProtectedAdmin: true}
	result := h.TestConnection(t.Context(), principal, ConnectionTest{Model: "chat-model", Backend: "target", API: "chat"})
	if result.OK || result.HTTPStatus != 503 || strings.Contains(result.Message, "private-upstream") || failed.Load() != 1 || other.Load() != 0 {
		t.Fatalf("unpinned diagnostic: %+v; target=%d other=%d", result, failed.Load(), other.Load())
	}
	for _, test := range []ConnectionTest{
		{Model: "chat-model", Backend: "absent", API: "chat"},
		{Model: "absent", Backend: "target", API: "chat"},
		{Model: "chat-model", URL: good.URL, API: "chat"}, // URL cannot name a reusable backend.
		{Model: "chat-model", Backend: "target", API: "embeddings"},
	} {
		if h.TestConnection(t.Context(), principal, test).OK {
			t.Fatal("invalid diagnostic accepted", test)
		}
	}
	if failed.Load() != 1 || other.Load() != 0 {
		t.Fatal("invalid selection made a provider request")
	}
}

func TestConnectionPermissionsAndInvalidSuccess(t *testing.T) {
	for _, response := range []string{`{"status":"ok"}`, `{"choices":[{}]}`, strings.Repeat("x", 70<<10)} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, response)
		}))
		cfg := testConfig(backend.URL)
		h, _ := configured(t, cfg)
		test := ConnectionTest{Model: "chat-model", URL: backend.URL, API: "chat"}
		denied := h.TestConnection(t.Context(), auth.Principal{UserID: "unknown"}, test)
		if denied.OK || denied.HTTPStatus != 403 {
			t.Fatalf("permissions bypassed: %+v", denied)
		}
		result := h.TestConnection(t.Context(), auth.Principal{UserID: "admin", ProtectedAdmin: true}, test)
		if result.OK {
			t.Fatal("health response or oversized body passed as inference", result)
		}
		backend.Close()
	}
}

func TestConnectionCancellationReleasesBackend(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(200)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer backend.Close()
	h, _ := setup(t, backend.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan ConnectionResult, 1)
	go func() {
		result <- h.TestConnection(ctx, auth.Principal{UserID: "admin", ProtectedAdmin: true}, ConnectionTest{Model: "chat-model", URL: backend.URL, API: "chat"})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test did not reach provider")
	}
	cancel()
	select {
	case got := <-result:
		if got.OK || !strings.Contains(got.Message, "canceled") {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("test did not stop")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("provider was not canceled")
	}
	for _, pool := range h.Pools.Pools {
		for _, replica := range pool.Snapshot() {
			if replica.Inflight != 0 {
				t.Fatal("capacity leaked", replica)
			}
		}
	}
}
