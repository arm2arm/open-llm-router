package integration_test

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRejectedCallsDoNotCaptureOrConsumeTraceBudget(t *testing.T) {
	var traces *requesttrace.Service
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), usageOptions{
		group: &config.Group{Models: []string{"chat-model"}, RPM: limitPtr(1)},
		configure: func(cfg *config.Config, p *proxy.Handler, _ *server.Server, pool *pgxpool.Pool) {
			cfg.Models["forbidden"] = cfg.Models["chat-model"]
			var err error
			traces, err = requesttrace.New(t.Context(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)), config.ContentTracing{Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(traces.Close)
			p.Traces = traces
		},
	})
	if err := traces.Save(t.Context(), f.owner, requesttrace.Preferences{Enabled: true, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct {
		body   string
		status int
	}{
		{`{"model":`, 400},
		{`{"model":"missing","messages":[{"role":"user","content":"rejected-private"}]}`, 404},
		{`{"model":"forbidden","messages":[{"role":"user","content":"rejected-private"}]}`, 403},
	} {
		// More than both the per-minute and concurrent trace budgets: a
		// rejection flood must not prevent the one admitted call's capture.
		for range requesttrace.UserCapturesPerMinute + 1 {
			r := f.call(t, f.key, "POST", "/v1/chat/completions", call.body)
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			if r.StatusCode != call.status {
				t.Fatal("rejected status", r.StatusCode, call.status)
			}
		}
	}
	r := f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("admitted status", r.StatusCode)
	}
	for range requesttrace.UserCapturesPerMinute + 1 {
		r := f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != 429 {
			t.Fatal("rate rejection", r.StatusCode)
		}
	}
	waitFor(t, func() bool { return traces.Processed.Load() == 1 })
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || traces.Dropped.Load() != 0 || traces.Failed.Load() != 0 {
		t.Fatal("rejections entered content storage", count, traces.Dropped.Load(), traces.Failed.Load())
	}
}

func TestTraceRetainedBudgetSerializesAcrossReplicas(t *testing.T) {
	f := startPortal(t)
	browser := f.browser(t)
	user := browser.login(t, "trace-budget", []string{"staff"})
	if err := f.traces.Save(t.Context(), user, requesttrace.Preferences{Enabled: true, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	second, err := requesttrace.New(t.Context(), f.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), config.ContentTracing{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	seed := func(count int) {
		t.Helper()
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail,summary) SELECT 'seed-'||n::text,$1,now(),now()+interval '1 day','{}','{}' FROM generate_series(1,$2::int) n`, user, count); err != nil {
			t.Fatal(err)
		}
	}
	seed(requesttrace.MaxStoredTraces - 1)
	var wg sync.WaitGroup
	for i, service := range []*requesttrace.Service{f.traces, second} {
		wg.Go(func() {
			capture := service.Begin(user, "chat/completions", time.Now())
			if capture == nil {
				t.Error("capture not admitted")
				return
			}
			capture.Input([]byte(`{"messages":[{"role":"user","content":"ordinary text"}]}`))
			capture.Finish(usage.Record{UserID: user, RequestID: fmt.Sprint("concurrent-", i), StartedAt: time.Now()})
		})
	}
	wg.Wait()
	waitFor(t, func() bool { return f.traces.Processed.Load()+second.Processed.Load() == 2 })
	if f.traces.Written.Load()+second.Written.Load() != 1 || f.traces.Dropped.Load()+second.Dropped.Load() != 1 || f.traces.Failed.Load()+second.Failed.Load() != 0 {
		t.Fatal("replicas overspent the retained trace budget")
	}
	if _, err := f.pool.Exec(t.Context(), `DELETE FROM request_traces WHERE user_id=$1`, user); err != nil {
		t.Fatal(err)
	}
	// Exercise the independent byte limit. Server-side repeat compresses well
	// and avoids uploading a 64 MiB fixture through the inference path.
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail,summary) VALUES('byte-budget',$1,now(),now()+interval '1 day',jsonb_build_object('padding',repeat('x',$2::int)),'{}')`, user, requesttrace.MaxStoredBytes-512); err != nil {
		t.Fatal(err)
	}
	capture := second.Begin(user, "chat/completions", time.Now())
	capture.Input([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("ordinary text ", 100) + `"}]}`))
	capture.Finish(usage.Record{UserID: user, RequestID: "byte-denied", StartedAt: time.Now()})
	waitFor(t, func() bool { return second.Processed.Load()+f.traces.Processed.Load() == 3 })
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces WHERE user_id=$1`, user).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || f.traces.Dropped.Load()+second.Dropped.Load() != 2 || second.Failed.Load() != 0 {
		t.Fatal("retained byte limit not enforced", count)
	}
	if _, err := f.traces.Erase(t.Context(), user, false); err != nil {
		t.Fatal(err)
	}
	// Refresh the second replica's generation after erasure and verify space
	// is reusable rather than permanently consuming a counter.
	if err := second.Save(t.Context(), user, requesttrace.Preferences{Enabled: true, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	capture = second.Begin(user, "chat/completions", time.Now())
	capture.Finish(usage.Record{UserID: user, RequestID: "after-erasure", StartedAt: time.Now()})
	waitFor(t, func() bool { return f.traces.Written.Load()+second.Written.Load() == 2 })
}
