package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

func limitPtr[T any](v T) *T { return &v }

func (f *usageRouter) call(t *testing.T, key, method, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const limitedBody = `{"model":"alias","messages":[{"role":"user","content":"hello private prompt-marker"}],"max_tokens":7}`

func (f *usageRouter) currentUsage(t *testing.T) limits.Snapshot {
	t.Helper()
	resp := f.call(t, f.key, "GET", "/v1/me/usage", "")
	defer resp.Body.Close()
	var s limits.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || resp.StatusCode != 200 {
		t.Fatalf("usage status=%d err=%v", resp.StatusCode, err)
	}
	return s
}

func requireLimit(t *testing.T, resp *http.Response, name string, anthropic bool) {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Type  string
		Error struct {
			Type, Limit, Message string
			ResetAt              time.Time `json:"reset_at"`
		}
	}
	err := json.NewDecoder(resp.Body).Decode(&body)
	if err != nil || resp.StatusCode != 429 || body.Error.Type != "rate_limit_error" || body.Error.Limit != name || body.Error.ResetAt.IsZero() || resp.Header.Get("Retry-After") == "" || !strings.Contains(body.Error.Message, name) || (anthropic && body.Type != "error") {
		t.Fatalf("limit response status=%d retry=%q body=%+v err=%v", resp.StatusCode, resp.Header.Get("Retry-After"), body, err)
	}
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for accounting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLimitsStreamingSlotsFinishAndDisconnect(t *testing.T) {
	finished := make(chan struct{}, 1)
	var calls atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finished:
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
		case <-r.Context().Done():
		}
	})
	f := startUsageRouter(t, backend, usageOptions{group: &config.Group{Models: []string{"*"}, MaxConcurrent: limitPtr(2)}})
	body := strings.TrimSuffix(limitedBody, "}") + `,"stream":true}`
	a := f.call(t, f.key, "POST", "/v1/chat/completions", body)
	b := f.call(t, f.key, "POST", "/v1/chat/completions", body)
	if a.StatusCode != 200 || b.StatusCode != 200 || f.currentUsage(t).InFlight != 2 {
		t.Fatal("two streams did not acquire slots")
	}
	requireLimit(t, f.call(t, f.key, "POST", "/v1/messages", body), "max_concurrent", true)
	if calls.Load() != 2 {
		t.Fatal("rejected request reached backend")
	}
	finished <- struct{}{}
	// One of the two active streams finishes; don't depend on which goroutine
	// receives the signal. A snapshot observes the released slot.
	waitFor(t, func() bool { return f.currentUsage(t).InFlight == 1 })
	c := f.call(t, f.key, "POST", "/v1/chat/completions", body)
	if c.StatusCode != 200 {
		t.Fatal(c.StatusCode)
	}
	a.Body.Close()
	b.Body.Close()
	c.Body.Close()
	waitFor(t, func() bool { return f.currentUsage(t).InFlight == 0 })
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
	// Cancellation released every slot, and recorded partial output.
	finished <- struct{}{}
	d := f.call(t, f.key, "POST", "/v1/chat/completions", body)
	io.Copy(io.Discard, d.Body)
	d.Body.Close()
	waitFor(t, func() bool { return f.currentUsage(t).InFlight == 0 })
}

func TestLimitsRPMSharedAcrossKeysAndProtocols(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), usageOptions{group: &config.Group{Models: []string{"*"}, RPM: limitPtr(1)}})
	otherKey, err := (&auth.Admin{Pool: f.pool}).CreateKey(t.Context(), f.owner, "second", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.request(t, "/v1/chat/completions", "")
	resp := f.call(t, otherKey, "POST", "/v1/messages", limitedBody)
	retry, parseErr := strconv.Atoi(resp.Header.Get("Retry-After"))
	if parseErr != nil || retry < 1 || retry > 60 {
		t.Fatal("unexpected RPM retry", resp.Header.Get("Retry-After"))
	}
	requireLimit(t, resp, "rpm", true)
	requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody), "rpm", false)
	s := f.currentUsage(t)
	if *s.Used.RPM != 1 || *s.Remaining.RPM != 0 || s.ResetAt.RPM == nil || s.InFlight != 0 {
		t.Fatal(s)
	}
	// Model authorization runs before RPM, including after the user's RPM fills.
	resp = f.call(t, f.key, "POST", "/v1/chat/completions", strings.Replace(limitedBody, "alias", "unknown", 1))
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
}

func TestLimitsAuthenticationAndModelPolicyPrecedeGates(t *testing.T) {
	var calls atomic.Int32
	mock := mockllm.New(mockllm.Options{})
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			calls.Add(1)
		}
		mock.ServeHTTP(w, r)
	}), usageOptions{group: &config.Group{Models: []string{}, RPM: limitPtr(1)}})
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		resp := f.call(t, "invalid", "POST", path, limitedBody)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatal(resp.StatusCode)
		}
		resp = f.call(t, f.key, "POST", path, limitedBody)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatal(resp.StatusCode)
		}
	}
	resp := f.call(t, "invalid", "GET", "/v1/me/usage", "")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("usage endpoint must authenticate")
	}
	s := f.currentUsage(t)
	if calls.Load() != 0 || *s.Used.RPM != 0 || s.InFlight != 0 || len(s.Models) != 0 {
		t.Fatal(s, calls.Load())
	}
}

func TestLimitsDailyResetDurableRestartAndUsageEndpoint(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	var unix atomic.Int64
	unix.Store(time.Date(2026, 5, 5, 22, 30, 0, 0, time.UTC).Unix())
	clock := func() time.Time { return time.Unix(unix.Load(), 0) }
	group := config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(10)), TokensPerMonth: limitPtr(int64(100))}
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7, TokenDelay: time.Millisecond}), usageOptions{group: &group, location: location, clock: clock})
	f.request(t, "/v1/chat/completions", `,"stream":true`)
	waitFor(t, func() bool { return f.currentUsage(t).Used.Today == 10 })
	requireLimit(t, f.call(t, f.key, "POST", "/v1/messages", limitedBody), "tokens_per_day", true)
	s := f.currentUsage(t)
	if s.Timezone != "Europe/Berlin" || s.Used.Month != 10 || *s.Remaining.Today != 0 || len(s.Models) != 1 || s.Models[0].InputTokens != 3 || s.Models[0].OutputTokens != 7 || s.Models[0].AverageTTFBMS == nil || s.Models[0].AverageTPS == nil {
		t.Fatalf("%+v", s)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 2 }) // success + rejected metadata
	var date string
	var total int64
	if err := f.pool.QueryRow(t.Context(), `SELECT day::text, input_tokens+output_tokens FROM usage_daily WHERE user_id=$1 AND model='chat-model'`, f.owner).Scan(&date, &total); err != nil || date != "2026-05-06" || total != 10 {
		t.Fatal(date, total, err)
	}
	// A new manager has no in-memory state, as after a process restart.
	restarted := limits.New(f.pool, location, clock)
	p := policy.Resolve(map[string]config.Group{"staff": group}, []string{"staff"})
	durable, err := restarted.Snapshot(t.Context(), f.owner, p)
	if err != nil || durable.Used.Today != s.Used.Today || durable.Used.Month != s.Used.Month || len(durable.Models) != 1 || *durable.Models[0].AverageTPS != *s.Models[0].AverageTPS {
		t.Fatalf("restart=%+v before=%+v err=%v", durable, s, err)
	}
	if l, err := restarted.Acquire(t.Context(), f.owner, p); err == nil {
		l.Release()
		t.Fatal("restart refilled quota")
	}
	unix.Add(24 * 3600)
	f.request(t, "/v1/messages", "")
	waitFor(t, func() bool { return f.currentUsage(t).Used.Month == 20 })
	s = f.currentUsage(t)
	if s.Used.Today != 10 || s.Models[0].AverageTPS != nil {
		t.Fatal(s)
	}
}

func TestLimitsMonthlyQuotaLoadsDailyAcrossModels(t *testing.T) {
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	group := config.Group{Models: []string{"*"}, TokensPerMonth: limitPtr(int64(100))}
	f := startUsageRouter(t, mockllm.New(mockllm.Options{}), usageOptions{group: &group, clock: func() time.Time { return now }})
	execSQL(t, f.pool, `INSERT INTO usage_daily(day,user_id,model,requests,input_tokens,output_tokens) VALUES
		('2026-05-01',$1,'other-model',1,30,40),('2026-05-05',$1,'chat-model',1,10,20),('2026-04-30',$1,'old-month',1,999,999)`, f.owner)
	requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody), "tokens_per_month", false)
	s := f.currentUsage(t)
	if s.Used.Today != 30 || s.Used.Month != 100 || *s.Remaining.Month != 0 || s.Remaining.Today != nil || len(s.Models) != 1 || s.Models[0].AverageTTFBMS != nil {
		t.Fatal(s)
	}
}

func TestLimitsTimingAveragesAndUserIsolation(t *testing.T) {
	now := time.Now()
	f := startUsageRouter(t, mockllm.New(mockllm.Options{}), usageOptions{clock: func() time.Time { return now }})
	other := user(t, f.pool, "local", "another-user")
	for _, r := range []usage.Record{
		{UserID: f.owner, Model: "a", InputTokens: 3, OutputTokens: 7, TTFBMS: limitPtr(int64(50)), LatencyMS: 400, IsStream: true},
		{UserID: f.owner, Model: "a", InputTokens: 5, OutputTokens: 100, TTFBMS: limitPtr(int64(100)), LatencyMS: 101}, // not a stream: excluded from TPS
		{UserID: f.owner, Model: "z", InputTokens: 2, OutputTokens: 5},
		{UserID: other, Model: "private", InputTokens: 999, OutputTokens: 999},
	} {
		r.StartedAt = now
		r.Protocol = "openai"
		r.Status = "200"
		f.writer.Record(r)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 4 })
	s := f.currentUsage(t)
	if s.Used.Today != 122 || s.Used.Month != 122 || len(s.Models) != 2 || s.Models[0].Model != "a" || *s.Models[0].AverageTTFBMS != 75 || *s.Models[0].AverageTPS != 20 || s.Models[1].Model != "z" || s.Models[1].AverageTPS != nil {
		t.Fatal(s)
	}
}

func TestLimitsStorageFailureReturnsSanitized503(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{}), usageOptions{group: &config.Group{Models: []string{"*"}, MaxConcurrent: limitPtr(1)}})
	execSQL(t, f.pool, `ALTER TABLE usage_daily RENAME TO unavailable_daily`)
	resp := f.call(t, f.key, "POST", "/v1/messages", limitedBody)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 503 || strings.Contains(string(body), "usage_daily") || !strings.Contains(string(body), "temporarily unavailable") || resp.Header.Get("Retry-After") != "1" {
		t.Fatal(resp.StatusCode, string(body), err)
	}
	execSQL(t, f.pool, `ALTER TABLE unavailable_daily RENAME TO usage_daily`)
	f.request(t, "/v1/messages", "") // failure did not leak a concurrency slot or poison the cache
}

func TestLimitsQuotaRemainsChargedOnAccountingFailure(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(10))}})
	execSQL(t, f.pool, `ALTER TABLE usage_daily ADD CONSTRAINT reject_test CHECK (requests < 0)`)
	f.request(t, "/v1/chat/completions", "")
	waitFor(t, func() bool { return f.writer.Counters().Failed == 1 })
	if f.currentUsage(t).Used.Today != 10 {
		t.Fatal("failed write refunded quota")
	}
	requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody), "tokens_per_day", false)
}

func TestLimitsRouterRestartRestoresQuota(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	backend := httptest.NewServer(mockllm.New(mockllm.Options{Tokens: 7}))
	defer backend.Close()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, fmt.Appendf(nil, `limits: {timezone: Europe/Berlin}
models:
  chat-model:
    protocols: [openai, anthropic]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 4}
groups:
  staff: {models: [chat-model], tokens_per_day: 10, tokens_per_month: 100}
aliases: {alias: chat-model}
`, backend.URL), 0600); err != nil {
		t.Fatal(err)
	}
	admin := auth.Admin{Pool: pool}
	owner, err := admin.AddUser(t.Context(), "restart", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := admin.CreateKey(t.Context(), owner, "restart", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base, stop := startRouter(t, binary, configPath, database.dsn)
	call := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	resp := call("POST", "/v1/chat/completions", limitedBody)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	stop() // graceful shutdown must flush the completed request before restart
	base, _ = startRouter(t, binary, configPath, database.dsn)
	resp = call("GET", "/v1/me/usage", "")
	var s limits.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || resp.StatusCode != 200 || s.Used.Today != 10 || s.Used.Month != 10 || s.Timezone != "Europe/Berlin" {
		t.Fatal(s, resp.StatusCode, err)
	}
	resp.Body.Close()
	requireLimit(t, call("POST", "/v1/messages", limitedBody), "tokens_per_day", true)
}

func TestLimitsCountTokensReleasesWithoutQuotaCharge(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), usageOptions{group: &config.Group{Models: []string{"*"}, MaxConcurrent: limitPtr(1)}})
	f.request(t, "/v1/messages/count_tokens", "")
	waitFor(t, func() bool { return f.currentUsage(t).InFlight == 0 })
	if f.currentUsage(t).Used.Today != 0 {
		t.Fatal("count_tokens charged quota")
	}
	f.stop()
	var rows int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM usage`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal(rows, err)
	}
}

// An override changes one user's limit on every router at the next request.
func TestOverridesApplyWithoutRestart(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 3}), usageOptions{group: &config.Group{Models: []string{"*"}, RPM: limitPtr(100)}})
	admin := auth.Admin{Pool: f.pool}
	other, err := admin.AddUser(t.Context(), "other", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := admin.CreateKey(t.Context(), other, "other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Warm both API-key caches first.
	for _, key := range []string{f.key, otherKey} {
		if resp := f.call(t, key, "POST", "/v1/chat/completions", limitedBody); resp.StatusCode != 200 {
			t.Fatal("warm-up", resp.StatusCode)
		}
	}
	if err := admin.SetOverride(t.Context(), f.owner, policy.FieldRPM, limitPtr(int64(1)), "abuse ticket 7", "operator"); err != nil {
		t.Fatal(err)
	}
	if s := f.currentUsage(t); s.Limits.RPM == nil || *s.Limits.RPM != 1 {
		t.Fatal("override not visible in /v1/me/usage")
	}
	requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody), "rpm", false)
	if resp := f.call(t, otherKey, "POST", "/v1/chat/completions", limitedBody); resp.StatusCode != 200 {
		t.Fatal("override affected another user", resp.StatusCode)
	}
	list, err := admin.ListOverrides(t.Context(), f.owner)
	if err != nil || len(list) != 1 || list[0].Field != "rpm" || *list[0].Value != 1 || list[0].Reason != "abuse ticket 7" || list[0].SetBy != "operator" {
		t.Fatal("override list", list, err)
	}
	// Unlimited lifts the group's limit; clearing returns to it.
	if err := admin.SetOverride(t.Context(), f.owner, policy.FieldRPM, nil, "", "operator"); err != nil {
		t.Fatal(err)
	}
	if s := f.currentUsage(t); s.Limits.RPM != nil {
		t.Fatal("unlimited override", *s.Limits.RPM)
	}
	if err := admin.ClearOverride(t.Context(), f.owner, policy.FieldRPM); err != nil {
		t.Fatal(err)
	}
	if s := f.currentUsage(t); s.Limits.RPM == nil || *s.Limits.RPM != 100 {
		t.Fatal("cleared override")
	}
	if err := admin.SetOverride(t.Context(), f.owner, "groups", limitPtr(int64(1)), "", ""); err == nil {
		t.Fatal("unknown field accepted")
	}
	if !errors.Is(admin.ClearOverride(t.Context(), f.owner, policy.FieldRPM), auth.ErrNotFound) {
		t.Fatal("clearing a missing override must report not found")
	}
}

// In strict mode concurrent calls cannot use more than the daily quota.
func TestStrictQuotaUnderConcurrency(t *testing.T) {
	const quota = 2000
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 50, TokenDelay: 5 * time.Millisecond}), usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(quota)), StrictQuota: true}})
	body := `{"model":"chat-model","messages":[{"role":"user","content":"hi"}],"max_tokens":50,"stream":true}`
	if resp := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"chat-model","messages":[]}`); resp.StatusCode != 400 {
		t.Fatal("strict mode without max_tokens", resp.StatusCode)
	}
	var ok, limited atomic.Int64
	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+f.key)
			resp, err := f.router.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case 200:
				ok.Add(1)
			case 429:
				limited.Add(1)
			default:
				t.Error("status", resp.StatusCode)
			}
		})
	}
	wg.Wait()
	waitFor(t, func() bool { return f.writer.Counters().Written == 61 }) // rejections are recorded too
	var used int64
	if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(sum(input_tokens + output_tokens), 0) FROM usage_daily WHERE user_id = $1`, f.owner).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used > quota || ok.Load() == 0 || limited.Load() == 0 {
		t.Fatalf("used %d of %d with %d calls admitted, %d limited", used, quota, ok.Load(), limited.Load())
	}
	if s := f.currentUsage(t); s.Reserved != 0 || s.InFlight != 0 {
		t.Fatal("reservations leaked", s.Reserved, s.InFlight)
	}
}

func TestBatchKeysHaveTheirOwnConcurrency(t *testing.T) {
	gate := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	f := startUsageRouter(t, slow, usageOptions{group: &config.Group{Models: []string{"*"}, MaxConcurrent: limitPtr(1), MaxConcurrentBatch: limitPtr(1)}})
	defer close(gate)
	batchKey, err := (auth.Admin{Pool: f.pool}).CreateKeyOfClass(t.Context(), f.owner, "batch", "batch", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan *http.Response, 2)
	for _, key := range []string{f.key, batchKey} {
		go func() {
			req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model"}`))
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := f.router.Client().Do(req)
			if err == nil {
				started <- resp
			}
		}()
	}
	waitFor(t, func() bool { s := f.currentUsage(t); return s.InFlight == 2 && s.Batch == 1 })
	requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"chat-model"}`), "max_concurrent", false)
	requireLimit(t, f.call(t, batchKey, "POST", "/v1/chat/completions", `{"model":"chat-model"}`), "max_concurrent_batch", false)
}

func TestTPMLimitThroughBothProtocols(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 30}), usageOptions{group: &config.Group{Models: []string{"*"}, TPM: limitPtr(int64(20))}})
	if resp := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"chat-model","messages":[]}`); resp.StatusCode != 200 {
		t.Fatal("first call", resp.StatusCode)
	}
	requireLimit(t, f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"chat-model","messages":[]}`), "tpm", false)
	requireLimit(t, f.call(t, f.key, "POST", "/v1/messages", `{"model":"chat-model","messages":[],"max_tokens":5}`), "tpm", true)
	if s := f.currentUsage(t); s.Limits.TPM == nil || *s.Limits.TPM != 20 || s.Remaining.TPM == nil || *s.Remaining.TPM != 0 {
		t.Fatal("tpm snapshot", s.Limits.TPM, s.Remaining.TPM)
	}
}
