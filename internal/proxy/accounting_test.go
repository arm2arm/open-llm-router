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
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

type recordChannel chan usage.Record

func (c recordChannel) Record(r usage.Record) { c <- r }

func (c recordChannel) next(t *testing.T) usage.Record {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("inference request was not recorded")
		return usage.Record{}
	}
}

func accountingRouter(t *testing.T, cfg *config.Config, recorder usage.Recorder) (*Handler, *httptest.Server) {
	t.Helper()
	h := New(cfg)
	h.Usage = recorder
	h.Pools.Check(t.Context())
	s := server.New(slog.New(slog.DiscardHandler))
	s.SetAPIMiddleware(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.NewContext(r.Context(), auth.Principal{UserID: "user", KeyID: "key", Groups: []string{"staff"}})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	h.Register(s.Handle)
	router := httptest.NewServer(s.Handler())
	t.Cleanup(func() { router.Close(); h.Close() })
	return h, router
}

func TestAccountingBothProtocolsAndFiltering(t *testing.T) {
	mock := mockllm.New(mockllm.Options{Tokens: 7})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	cfg := testConfig(backend.URL)
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
	records := make(recordChannel, 16)
	_, router := accountingRouter(t, cfg, records)
	for _, tc := range []struct {
		path, extra string
		output      int64
		usageChunk  bool
	}{
		{"/v1/chat/completions", "", 7, true},
		{"/v1/chat/completions", `,"stream":true`, 7, false},
		{"/v1/chat/completions", `,"stream":true,"stream_options":{"include_usage":false,"future":123456789012345678901234567890}`, 7, false},
		{"/v1/chat/completions", `,"stream":true,"stream_options":{"include_usage":true}`, 7, true},
		{"/v1/completions", "", 7, true},
		{"/v1/completions", `,"stream":true`, 7, false},
		{"/v1/completions", `,"stream":true,"stream_options":{"include_usage":true}`, 7, true},
		{"/v1/embeddings", "", 0, true},
		{"/v1/messages", "", 7, true},
		{"/v1/messages", `,"stream":true`, 7, true},
	} {
		t.Run(tc.path+tc.extra, func(t *testing.T) {
			resp, body := send(t, router.URL, tc.path, `{"model":"alias","messages":[{"role":"user","content":"hello private-marker"}],"prompt":"hello private-marker","input":"hello private-marker","max_tokens":7`+tc.extra+`}`, nil)
			if resp.StatusCode != 200 || bytes.Contains(body, []byte(`"usage"`)) != tc.usageChunk {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			r := records.next(t)
			if r.UserID != "user" || r.KeyID != "key" || r.Model != "chat-model" || r.Replica != backend.URL || r.Status != "200" || r.InputTokens != 2 || r.OutputTokens != tc.output ||
				r.TTFBMS == nil || *r.TTFBMS < 0 || r.LatencyMS < *r.TTFBMS {
				t.Fatalf("accounting = %+v", r)
			}
			if strings.Contains(tc.extra, `"stream":true`) && !strings.Contains(tc.path, "/messages") {
				var upstream struct {
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				if json.Unmarshal(mock.Stats().Last.Body, &upstream) != nil || !upstream.StreamOptions.IncludeUsage {
					t.Fatal("did not request upstream usage")
				}
				if strings.Contains(tc.extra, "future") && !bytes.Contains(mock.Stats().Last.Body, []byte(`123456789012345678901234567890`)) {
					t.Fatal("changed an unknown stream option")
				}
			}
		})
	}
	for _, path := range []string{"/v1/models", "/v1/messages/count_tokens"} {
		if path == "/v1/models" {
			resp, err := http.Get(router.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		} else {
			resp, _ := send(t, router.URL, path, `{"model":"chat-model","messages":[]}`, nil)
			if resp.StatusCode != 200 {
				t.Fatal(resp.StatusCode)
			}
		}
	}
	router.Close()
	if len(records) != 0 {
		t.Fatal("count_tokens, models, or a duplicate call was recorded")
	}
}

func TestAccountingErrorsAndAbort(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, failure := range []mockllm.Fail{mockllm.FailError, mockllm.FailDropMidStream, mockllm.FailNone} {
			t.Run(path+string(failure), func(t *testing.T) {
				mock := mockllm.New(mockllm.Options{Tokens: 20, TokenDelay: 5 * time.Millisecond, Fail: failure})
				backend := httptest.NewServer(mock)
				defer backend.Close()
				cfg := testConfig(backend.URL)
				cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
				records := make(recordChannel, 1)
				_, router := accountingRouter(t, cfg, records)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req, _ := http.NewRequestWithContext(ctx, "POST", router.URL+path, strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}],"max_tokens":20,"stream":true}`))
				resp, err := router.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if failure == mockllm.FailNone {
					reader := bufio.NewReader(resp.Body)
					content := 0
					for content < 3 {
						line, err := reader.ReadString('\n')
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(line, `"content":" `) || strings.Contains(line, `"type":"text_delta"`) {
							content++
						}
					}
					cancel()
				} else {
					_, _ = io.Copy(io.Discard, resp.Body)
				}
				resp.Body.Close()
				r := records.next(t)
				if failure == mockllm.FailError {
					if r.Status != "500" || r.OutputTokens != 0 {
						t.Fatal(r)
					}
				} else if r.Status != "aborted" || r.OutputTokens < 3 || r.OutputTokens >= 20 {
					t.Fatal("abort was not accounted with a content estimate", r)
				}
				if failure == mockllm.FailDropMidStream && r.OutputTokens != 10 {
					t.Fatal("broken stream content estimate", r)
				}
			})
		}
	}
}

func TestAccountingGenerationRate(t *testing.T) {
	// Longer token intervals keep scheduler/timer granularity well below the
	// 10% tolerance, including under the race detector on macOS.
	mock := mockllm.New(mockllm.Options{Tokens: 30, TokenDelay: 50 * time.Millisecond})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	cfg := testConfig(backend.URL)
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
	records := make(recordChannel, 1)
	_, router := accountingRouter(t, cfg, records)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		resp, _ := send(t, router.URL, path, `{"model":"chat-model","messages":[],"max_tokens":30,"stream":true}`, nil)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		r := records.next(t)
		if rate := r.TokensPerSecond(); rate < 18 || rate > 22 {
			t.Fatalf("rate=%v tokens/s, want 20 ±10%%: %+v", rate, r)
		}
	}
}

func TestAccountingFailoverRecordsOnce(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			panic(http.ErrAbortHandler)
		}
	}))
	defer failed.Close()
	good := httptest.NewServer(mockllm.New(mockllm.Options{Tokens: 7}))
	defer good.Close()
	cfg := testConfig(failed.URL, good.URL)
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
	records := make(recordChannel, 4)
	h, router := accountingRouter(t, cfg, records)
	pool := h.Pools.Pools["chat-model"]
	lease := pool.Acquire(nil)
	for lease.Replica.URL != good.URL {
		lease.Release()
		lease = pool.Acquire(nil)
	}
	defer lease.Release() // Forces the first selection onto the failing replica.
	resp, _ := send(t, router.URL, "/v1/chat/completions", `{"model":"alias","messages":[]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	router.Close()
	r := records.next(t)
	if r.Status != "200" || r.Replica != good.URL || r.OutputTokens != 7 || len(records) != 0 {
		t.Fatal("failover recorded more than one call or the wrong replica", r)
	}
}
