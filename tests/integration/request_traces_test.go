package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/reqid"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTraceStorageDoesNotHoldStreamingResponse(t *testing.T) {
	release := make(chan struct{})
	var traces *requesttrace.Service
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}), usageOptions{configure: func(_ *config.Config, p *proxy.Handler, _ *server.Server, pool *pgxpool.Pool) {
		var err error
		traces, err = requesttrace.New(t.Context(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(traces.Close)
		p.Traces = traces
	}})
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `LOCK TABLE request_traces IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","stream":true,"max_tokens":7,"messages":[{"role":"user","content":"synthetic"}]}`))
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	response, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data:") {
		t.Fatal("first event unavailable", err)
	}
	// The provider cannot finish until AFTER the client receives its first event.
	close(release)
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return traces.Processed.Load() == 1 })
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces WHERE user_id=$1`, f.owner).Scan(&count); err != nil || count != 1 {
		t.Fatal("background capture did not complete", count, err)
	}
}

func TestPersonalRequestTraces(t *testing.T) {
	f := startPortal(t)
	a, b, root := f.browser(t), f.browser(t), f.browser(t)
	alice := a.login(t, "trace-alice", []string{"staff"})
	b.login(t, "trace-bob", []string{"staff"})
	root.login(t, "trace-admin", []string{"admins"})
	resp, data := a.call(t, "GET", "/portal/api/trace-preferences", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"enabled":true`) || !strings.Contains(string(data), `"retention_days":7`) {
		t.Fatalf("defaults: %d %s", resp.StatusCode, data)
	}
	key, err := (auth.Admin{Pool: f.pool}).CreateKey(t.Context(), alice, "trace-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	call := func(stream bool) string {
		t.Helper()
		body := `{"model":"chat-model","messages":[{"role":"user","content":"trace-private-input"}],"max_tokens":7,"stream":false}`
		if stream {
			body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
		}
		req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Private-Header", "secret-header-never-stored")
		r, err := f.router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		_, err = io.Copy(io.Discard, r.Body)
		if err != nil || r.StatusCode != 200 {
			t.Fatal("inference failed", r.StatusCode, err)
		}
		return r.Header.Get("X-Request-Id")
	}
	count := func() int {
		var n int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces WHERE user_id=$1`, alice).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	id := call(true)
	waitFor(t, func() bool { return count() == 1 })
	// The requests list reads usage rows, which the accounting writer stores
	// asynchronously and independently of the trace.
	waitFor(t, func() bool {
		var n int
		return f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE request_id=$1`, id).Scan(&n) == nil && n == 1
	})
	resp, data = a.call(t, "GET", "/portal/api/requests/"+id, "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(data), "trace-private-input") || !strings.Contains(string(data), `"events":[{`) || strings.Contains(string(data), key) || strings.Contains(string(data), "secret-header-never-stored") {
		t.Fatalf("trace: %d %s", resp.StatusCode, data)
	}
	// Version 2: raw request, routing by pool position, header names only.
	for _, want := range []string{`"v":2`, `"request":{`, `"replica 1 of 1"`, `"X-Private-Header"`, `"upstream_status":200`, `"frames":[{`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("trace lacks %s: %s", want, data)
		}
	}
	if strings.Contains(string(data), "127.0.0.1") {
		t.Fatal("trace exposed a backend address")
	}
	if resp, list := a.call(t, "GET", "/portal/api/requests", "", ""); resp.StatusCode != 200 || !strings.Contains(string(list), `"input_messages":1`) || !strings.Contains(string(list), `"conversation":"c`) {
		t.Fatalf("list summary: %s", list)
	}
	var result struct {
		Expires time.Time `json:"expires_at"`
	}
	if json.Unmarshal(data, &result) != nil || time.Until(result.Expires) < 6*24*time.Hour || time.Until(result.Expires) > 7*24*time.Hour {
		t.Fatal("default expiry", result.Expires)
	}
	for _, viewer := range []*portalBrowser{b, root, f.browser(t)} {
		want := 404
		if viewer != b && viewer != root {
			want = 401
		}
		if r, _ := viewer.call(t, "GET", "/portal/api/requests/"+id, "", ""); r.StatusCode != want {
			t.Fatal("cross-account content access", r.StatusCode)
		}
	}
	for _, body := range []string{`{"enabled":true,"retention_days":0}`, `{"enabled":true,"retention_days":31}`, `{"retention_days":7}`} {
		r, _ := a.call(t, "PUT", "/portal/api/trace-preferences", body, f.cfg.Portal.BaseURL)
		if r.StatusCode != 400 {
			t.Fatal("invalid preferences accepted")
		}
	}
	if r, _ := a.call(t, "PUT", "/portal/api/trace-preferences", `{"enabled":false,"retention_days":14}`, "https://evil.test"); r.StatusCode != 403 {
		t.Fatal("cross-origin settings accepted")
	}
	// A capture begun before opting out must not be persisted after the change.
	late := f.traces.Begin(alice, "chat/completions", time.Now())
	late.Input([]byte(`{"messages":[{"role":"user","content":"late-private"}]}`))
	if r, _ := a.call(t, "PUT", "/portal/api/trace-preferences", `{"enabled":false,"retention_days":14}`, f.cfg.Portal.BaseURL); r.StatusCode != 200 {
		t.Fatal("opt out failed")
	}
	processed := f.traces.Processed.Load()
	late.Finish(usage.Record{RequestID: reqid.New(), UserID: alice, StartedAt: time.Now()})
	call(false)
	waitFor(t, func() bool { return f.traces.Processed.Load() > processed })
	if count() != 1 || f.traces.Begin(alice, "chat", time.Now()) != nil {
		t.Fatal("opt out stored new content")
	}
	// Existing content remains available until expiry or explicit deletion.
	if r, _ := a.call(t, "GET", "/portal/api/requests/"+id, "", ""); r.StatusCode != 200 {
		t.Fatal("opt out unexpectedly erased history")
	}
	if r, _ := a.call(t, "PUT", "/portal/api/trace-preferences", `{"enabled":true,"retention_days":3}`, f.cfg.Portal.BaseURL); r.StatusCode != 200 {
		t.Fatal("opt in failed")
	}
	id2 := call(false)
	waitFor(t, func() bool { return count() == 2 })
	if r, data := a.call(t, "GET", "/portal/api/requests/"+id2, "", ""); r.StatusCode != 200 || !strings.Contains(string(data), `"output":[{`) {
		t.Fatalf("JSON trace unavailable: %d %s", r.StatusCode, data)
	}
	late = f.traces.Begin(alice, "chat", time.Now())
	if r, _ := a.call(t, "DELETE", "/portal/api/request-traces", `{}`, f.cfg.Portal.BaseURL); r.StatusCode != 204 {
		t.Fatal("clear failed")
	}
	processed = f.traces.Processed.Load()
	late.Finish(usage.Record{RequestID: reqid.New(), UserID: alice, StartedAt: time.Now()})
	waitFor(t, func() bool { return f.traces.Processed.Load() > processed })
	if count() != 0 {
		t.Fatal("cleared content resurrected")
	}
	call(false)
	waitFor(t, func() bool { return count() == 1 })
	if r, _ := a.call(t, "DELETE", "/portal/api/usage", `{}`, f.cfg.Portal.BaseURL); r.StatusCode != 200 || count() != 0 {
		t.Fatal("activity erasure left trace content")
	}
	var totals int
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(requests) FROM usage_daily WHERE user_id=$1`, alice).Scan(&totals); err != nil || totals != 4 {
		t.Fatal("trace controls changed quota accounting", totals, err)
	}
}

func TestTraceRetentionAndUserErasure(t *testing.T) {
	f := startPortal(t)
	a := f.browser(t)
	alice := a.login(t, "retention-alice", []string{"staff"})
	id := reqid.New()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail) VALUES($1,$2,now()-interval '3 days',now()+interval '4 days','{}')`, id, alice); err != nil {
		t.Fatal(err)
	}
	if r, _ := a.call(t, "PUT", "/portal/api/trace-preferences", `{"enabled":true,"retention_days":1}`, f.cfg.Portal.BaseURL); r.StatusCode != 200 {
		t.Fatal("shorten retention")
	}
	if r, _ := a.call(t, "GET", "/portal/api/requests/"+id, "", ""); r.StatusCode != 404 {
		t.Fatal("expired content accessible")
	}
	if err := f.traces.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces WHERE user_id=$1`, alice).Scan(&n)
	if n != 0 {
		t.Fatal("expired content not pruned")
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail) VALUES($1,$2,now(),now()+interval '1 day','{}')`, id, alice); err != nil {
		t.Fatal(err)
	}
	if err := (auth.Admin{Pool: f.pool}).DeleteUser(t.Context(), alice); err != nil {
		t.Fatal(err)
	}
	_ = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM request_traces WHERE user_id=$1`, alice).Scan(&n)
	if n != 0 {
		t.Fatal("user erasure left trace content")
	}
}

func TestForwardedTraceBelongsToAttributedUser(t *testing.T) {
	f := startPortal(t, usageOptions{forwarders: &config.Forwarders{UnknownUser: config.UnknownUserReject, HeaderMap: map[string]string{"X-Olr-End-User": "username"}}})
	a, b := f.browser(t), f.browser(t)
	alice := a.login(t, "forwarded-trace-alice", []string{"staff"})
	bob := b.login(t, "forwarded-trace-bob", []string{"staff"})
	key, err := (auth.Admin{Pool: f.pool}).CreateForwarderKey(t.Context(), bob, "forwarder", "interactive", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[{"role":"user","content":"forwarded-private"}],"max_tokens":7}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Olr-End-User", "forwarded-trace-alice")
	resp, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("forwarded request", resp.StatusCode)
	}
	waitFor(t, func() bool { return f.traces.Processed.Load() == 1 })
	var owner string
	if err := f.pool.QueryRow(t.Context(), `SELECT user_id::text FROM request_traces`).Scan(&owner); err != nil || owner != alice {
		t.Fatal("wrong content owner", owner, err)
	}
	path := "/portal/api/requests/" + resp.Header.Get("X-Request-Id")
	if r, data := a.call(t, "GET", path, "", ""); r.StatusCode != 200 || !strings.Contains(string(data), "forwarded-private") {
		t.Fatal("attributed user missing trace")
	}
	if r, _ := b.call(t, "GET", path, "", ""); r.StatusCode != 404 {
		t.Fatal("forwarder's key owner read attributed content")
	}
}
