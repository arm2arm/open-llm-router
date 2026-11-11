package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5/pgxpool"
)

type usageRouter struct {
	pool   *pgxpool.Pool
	router *httptest.Server
	writer *usage.Writer
	logs   *authLog
	owner  string
	key    string
	stop   func()
}

type usageOptions struct {
	group      *config.Group
	location   *time.Location
	clock      func() time.Time
	configure  func(*config.Config, *proxy.Handler, *server.Server, *pgxpool.Pool)
	forwarders *config.Forwarders
}

func startUsageRouter(t *testing.T, handler http.Handler, options ...usageOptions) *usageRouter {
	t.Helper()
	f := &usageRouter{pool: openDatabase(t, newDatabase(t).dsn), logs: &authLog{}}
	backend := httptest.NewServer(handler)
	admin := auth.Admin{Pool: f.pool}
	var err error
	f.owner, err = admin.AddUser(t.Context(), "accounting", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	f.key, err = admin.CreateKey(t.Context(), f.owner, "accounting", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.New(t.Context(), f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) > 0 && options[0].forwarders != nil {
		authenticator.Forwarders = *options[0].forwarders
	}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	location, clock := time.UTC, time.Now
	if len(options) > 0 {
		if options[0].location != nil {
			location = options[0].location
		}
		if options[0].clock != nil {
			clock = options[0].clock
		}
	}
	f.writer = usage.NewWriterInLocation(f.pool, log, location)
	cfg := &config.Config{
		Auth:   config.Auth{DefaultKeyLifetimeDays: 90},
		Limits: config.Limits{Timezone: location.String(), Location: location},
		Models: map[string]config.Model{"chat-model": {
			Protocols: []string{"openai", "anthropic"}, TimeoutS: 3, IdleTimeoutS: 2,
			Pool: []config.Replica{{URL: backend.URL, Weight: 1, MaxInflight: 256}},
		}},
		Aliases: map[string]string{"alias": "chat-model"},
		Groups:  map[string]config.Group{"staff": {Models: []string{"*"}}},
	}
	if len(options) > 0 && options[0].group != nil {
		cfg.Groups["staff"] = *options[0].group
	}
	forwarder := proxy.New(cfg)
	forwarder.Usage = f.writer
	if len(options) > 0 {
		forwarder.Limits = limits.New(f.pool, location, clock)
	}
	forwarder.Pools.Check(t.Context())
	s := server.New(log)
	s.SetAPIMiddleware(authenticator.Middleware)
	forwarder.Register(s.Handle)
	if len(options) > 0 && options[0].configure != nil {
		options[0].configure(cfg, forwarder, s, f.pool)
	}
	f.router = httptest.NewServer(s.Handler())
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			f.router.Close()
			forwarder.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := f.writer.Close(ctx); err != nil {
				t.Error(err)
			}
			authenticator.Close()
			backend.Close()
		})
	}
	t.Cleanup(f.stop)
	return f
}

func (f *usageRouter) request(t *testing.T, path, extra string) []byte {
	t.Helper()
	method := "POST"
	if path == "/v1/models" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+path,
		strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello private prompt-marker"}],"prompt":"hello private prompt-marker","input":"hello private prompt-marker","max_tokens":7`+extra+`}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%s: %d %s err=%v", path, resp.StatusCode, body, err)
	}
	return body
}

func TestUsagePersistenceAndPrivacy(t *testing.T) {
	mock := mockllm.New(mockllm.Options{Tokens: 7})
	const completionMarker = "private-completion-marker"
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"content":%q,"usage":{"prompt_tokens":3,"total_tokens":3}}`, completionMarker)
			return
		}
		mock.ServeHTTP(w, r)
	}))
	for _, tc := range []struct {
		path, extra string
		output      int64
	}{
		{"/v1/chat/completions", "", 7},
		{"/v1/chat/completions", `,"stream":true`, 7},
		{"/v1/chat/completions", `,"stream":true,"stream_options":{"include_usage":true}`, 7},
		{"/v1/completions", "", 7},
		{"/v1/completions", `,"stream":true`, 7},
		{"/v1/embeddings", "", 0},
		{"/v1/messages", "", 7},
		{"/v1/messages", `,"stream":true`, 7},
	} {
		body := f.request(t, tc.path, tc.extra)
		if !strings.Contains(tc.extra, `"stream":true`) {
			var response struct{ Usage map[string]int64 }
			if json.Unmarshal(body, &response) != nil {
				t.Fatal("invalid backend JSON")
			}
			input, output := response.Usage["prompt_tokens"], response.Usage["completion_tokens"]
			if tc.path == "/v1/messages" {
				input, output = response.Usage["input_tokens"], response.Usage["output_tokens"]
			}
			if input != 3 || output != tc.output {
				t.Fatal("unexpected backend usage", response)
			}
		}
		if tc.path == "/v1/embeddings" && !bytes.Contains(body, []byte(completionMarker)) {
			t.Fatal("privacy response did not reach the client")
		}
	}
	f.request(t, "/v1/models", "")
	f.request(t, "/v1/messages/count_tokens", "")
	f.stop() // Drains all pending writes before reading rows.
	rows, err := f.pool.Query(t.Context(), `SELECT row_to_json(u)::text, input_tokens, output_tokens, status, model, ttfb_ms, latency_ms FROM usage u`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count, outputTotal := 0, int64(0)
	for rows.Next() {
		var persisted, status, model string
		var input, output, ttfb, latency int64
		if err := rows.Scan(&persisted, &input, &output, &status, &model, &ttfb, &latency); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(persisted, "prompt-marker") || strings.Contains(persisted, completionMarker) || strings.Contains(persisted, f.key) ||
			input != 3 || status != "200" || model != "chat-model" || ttfb < 0 || latency < ttfb {
			t.Fatal("incorrect or private accounting data", persisted)
		}
		outputTotal += output
		count++
	}
	if rows.Err() != nil || count != 8 || outputTotal != 49 {
		t.Fatalf("rows=%d output=%d err=%v", count, outputTotal, rows.Err())
	}
	if strings.Contains(f.logs.String(), "prompt-marker") || strings.Contains(f.logs.String(), completionMarker) || strings.Contains(f.logs.String(), f.key) {
		t.Fatal("request content or credentials appeared in logs")
	}
	if counts := f.writer.Counters(); counts.Written != 8 || counts.Failed != 0 || counts.Dropped != 0 {
		t.Fatal(counts)
	}
}

func TestUsageDailyMatches100ConcurrentRequests(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}))
	// Warm authentication before the concurrent phase so this test measures
	// inference and the accounting writer, not 100 simultaneous cold lookups.
	f.request(t, "/v1/models", "")
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			path, extra := "/v1/chat/completions", ""
			if i%2 == 0 {
				path = "/v1/messages"
			}
			if i%3 == 0 {
				extra = `,"stream":true`
			}
			f.request(t, path, extra)
		})
	}
	wg.Wait()
	f.stop()
	var requests, input, output int64
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*), sum(input_tokens), sum(output_tokens) FROM usage WHERE user_id=$1`, f.owner).
		Scan(&requests, &input, &output); err != nil {
		t.Fatal(err)
	}
	if requests != 100 || input != 300 || output != 700 {
		t.Fatalf("requests=%d input=%d output=%d", requests, input, output)
	}
	var dailyRequests, dailyInput, dailyOutput int64
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(requests), sum(input_tokens), sum(output_tokens) FROM usage_daily WHERE user_id=$1`, f.owner).
		Scan(&dailyRequests, &dailyInput, &dailyOutput); err != nil {
		t.Fatal(err)
	}
	if dailyRequests != requests || dailyInput != input || dailyOutput != output {
		t.Fatalf("daily=%d/%d/%d rows=%d/%d/%d", dailyRequests, dailyInput, dailyOutput, requests, input, output)
	}
	if counts := f.writer.Counters(); counts.Written != 100 || counts.Failed != 0 || counts.Dropped != 0 {
		t.Fatal(counts)
	}
}

func TestUsageTransactionRollsBack(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}))
	execSQL(t, f.pool, `ALTER TABLE usage_daily ADD CONSTRAINT reject_test CHECK (requests < 0)`)
	f.request(t, "/v1/chat/completions", "") // Storage failure must not alter the 200 response.
	f.stop()
	var rows int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage`).Scan(&rows); err != nil || rows != 0 || f.writer.Counters().Failed != 1 {
		t.Fatalf("transaction left %d usage rows; counts=%+v err=%v", rows, f.writer.Counters(), err)
	}
}

func TestUsageAbortedStreamPersists(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 20, TokenDelay: time.Millisecond, Fail: mockllm.FailDropMidStream}))
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+path,
			strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello private prompt-marker"}],"max_tokens":20,"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.key)
		resp, err := f.router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Fatal("broken stream appeared complete")
		}
	}
	f.stop()
	var rows int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE status='aborted' AND output_tokens=10 AND ttfb_ms IS NOT NULL`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("aborted rows=%d, want 2; err=%v", rows, err)
	}
}
