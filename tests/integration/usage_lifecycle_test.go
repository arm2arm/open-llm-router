package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

func TestRejectedFloodCannotDisplaceCompletedUsage(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), usageOptions{bodyReaders: 128})
	admin := auth.Admin{Pool: f.pool}
	victim, err := admin.AddUser(t.Context(), "victim", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := admin.CreateKey(t.Context(), victim, "victim", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var rejected atomic.Int64
	var finished atomic.Bool
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for !finished.Load() || rejected.Load() < 3000 {
				req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"unavailable","messages":[]}`))
				req.Header.Set("Authorization", "Bearer "+f.key)
				resp, err := f.router.Client().Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 404 {
					t.Error("rejection status", resp.StatusCode)
					return
				}
				rejected.Add(1)
			}
		})
	}
	defer func() { finished.Store(true); wg.Wait() }()
	waitFor(t, func() bool { return rejected.Load() >= 1000 })
	for range 50 {
		resp := f.call(t, key, "POST", "/v1/chat/completions", limitedBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal("completed call", resp.StatusCode)
		}
	}
	finished.Store(true)
	wg.Wait()
	f.stop()
	var requests, tokens, attackRows int64
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE user_id=$1`, victim).Scan(&requests); err != nil || requests != 50 {
		t.Fatal("lost victim history", requests, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(requests),sum(input_tokens+output_tokens) FROM usage_daily WHERE user_id=$1`, victim).Scan(&requests, &tokens); err != nil || requests != 50 || tokens != 500 {
		t.Fatal("lost victim charges", requests, tokens, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE user_id=$1`, f.owner).Scan(&attackRows); err != nil || attackRows != 0 {
		t.Fatal("rejections consumed database writes", attackRows, err)
	}
	if counts := f.writer.Counters(); counts.Written != 50 || counts.Dropped != 0 || counts.Failed != 0 {
		t.Fatal(counts)
	}
}

func TestCompletionSurvivesKeyDeletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if backendProbe(r) {
			return
		}
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":3,"completion_tokens":7}}`)
	}))
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan int, 1)
	go func() {
		resp := f.call(t, f.key, "POST", "/v1/chat/completions", limitedBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	<-entered
	admin := auth.Admin{Pool: f.pool}
	keys, err := admin.ListKeys(t.Context(), f.owner)
	if err != nil || len(keys) != 1 {
		t.Fatal(keys, err)
	}
	if err := admin.RevokeKey(t.Context(), keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := admin.RemoveOwnedKey(t.Context(), f.owner, keys[0].ID); err != nil || !deleted {
		t.Fatal("delete running key", deleted, err)
	}
	unblock()
	if status := <-done; status != 200 {
		t.Fatal(status)
	}
	f.stop()
	var rows int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE user_id=$1 AND key_id IS NULL AND input_tokens=3 AND output_tokens=7`, f.owner).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("missing detached completion", rows, err)
	}
	if counts := f.writer.Counters(); counts.Written != 1 || counts.Failed != 0 {
		t.Fatal(counts)
	}
	// Reload from PostgreSQL as a fresh process does, including monthly charges.
	restarted := limits.New(f.pool, time.UTC, time.Now)
	snapshot, err := restarted.Snapshot(t.Context(), f.owner, policy.Resolve(map[string]config.Group{"staff": {Models: []string{"*"}}}, []string{"staff"}))
	if err != nil || snapshot.Used.Today != 10 || snapshot.Used.Month != 10 {
		t.Fatal(snapshot, err)
	}
	var dailyRequests int
	if err := f.pool.QueryRow(t.Context(), `SELECT sum(requests) FROM usage_daily WHERE user_id=$1`, f.owner).Scan(&dailyRequests); err != nil || dailyRequests != 1 {
		t.Fatal("daily request count", dailyRequests, err)
	}
}

func TestForwarderErasurePreservesEndUserHistory(t *testing.T) {
	f := startForwarder(t, config.UnknownUserReject)
	resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "alice"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	access := policy.Resolve(map[string]config.Group{"staff": {Models: []string{"*"}}}, []string{"staff"})
	snapshot := func() limits.Snapshot {
		t.Helper()
		s, err := limits.New(f.pool, time.UTC, time.Now).Snapshot(t.Context(), f.alice, access)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	admin := auth.Admin{Pool: f.pool}
	if err := admin.DeleteUser(t.Context(), f.webui); err != nil {
		t.Fatal(err)
	}
	after := snapshot()
	if !reflect.DeepEqual(before.Models, after.Models) || before.Used.Today != after.Used.Today || before.Used.Month != after.Used.Month {
		t.Fatal("forwarder erasure changed end-user history/timings", before, after)
	}
	if f.rows(t, `SELECT count(*) FROM usage WHERE user_id IS NULL AND key_id IS NULL AND forwarded_for=$1`, f.alice) != 1 {
		t.Fatal("forwarded call lost")
	}
	// A late completion also survives erasure of the forwarding account.
	f.writer.Record(usage.Record{StartedAt: time.Now(), UserID: f.alice, ForwardedBy: f.webui, Model: "chat-model", Protocol: "openai", Status: "200", InputTokens: 2, OutputTokens: 3})
	waitFor(t, func() bool { return f.writer.Counters().Written == 2 })
	if err := admin.DeleteUser(t.Context(), f.alice); err != nil {
		t.Fatal(err)
	}
	// Deleted end users must not reappear through late asynchronous records.
	f.writer.Record(usage.Record{StartedAt: time.Now(), UserID: f.alice, ForwardedBy: f.webui, Model: "chat-model", Protocol: "openai", Status: "200", InputTokens: 2, OutputTokens: 3})
	f.stop()
	if f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1 OR user_id=$1`, f.alice) != 0 || f.rows(t, `SELECT count(*) FROM usage_daily WHERE user_id=$1`, f.alice) != 0 || f.writer.Counters().Failed != 0 {
		t.Fatal("erased user retained or recreated", f.writer.Counters())
	}
}

func TestDirectUsageErasureAndLateCompletion(t *testing.T) {
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}))
	f.request(t, "/v1/chat/completions", "")
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	// The database-level contract also holds for erasure outside auth.Admin.
	execSQL(t, f.pool, `DELETE FROM users WHERE id=$1`, f.owner)
	f.writer.Record(usage.Record{StartedAt: time.Now(), UserID: f.owner, Model: "chat-model", Protocol: "openai", Status: "200", InputTokens: 3, OutputTokens: 7})
	f.stop()
	var rows int
	if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM usage)+(SELECT count(*) FROM usage_daily)`).Scan(&rows); err != nil || rows != 0 || f.writer.Counters().Failed != 0 {
		t.Fatal("erasure left accounting orphans", rows, f.writer.Counters(), err)
	}
}

func TestForwarderErasurePreservesPortalAndAPIHistory(t *testing.T) {
	f := startPortal(t, usageOptions{forwarders: emailMapping(config.UnknownUserReject)})
	browser := f.browser(t)
	alice := browser.login(t, "alice", []string{"staff"})
	forwarder := emailForwarder(t, f.usageRouter)
	resp := forwarder.send(t, forwarder.key, map[string]string{"X-OLR-End-User": "alice"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	history := func() map[string]any {
		t.Helper()
		resp, body := browser.call(t, "GET", "/portal/api/requests", "", "")
		var result map[string]any
		if resp.StatusCode != 200 || json.Unmarshal(body, &result) != nil {
			t.Fatal("portal history", resp.StatusCode, string(body))
		}
		rows, ok := result["requests"].([]any)
		if !ok || len(rows) != 1 {
			t.Fatal("missing forwarded history", result)
		}
		// Key provenance disappears when its owner is erased; call metadata stays.
		rows[0].(map[string]any)["key_prefix"] = nil
		return result
	}
	admin := auth.Admin{Pool: f.pool}
	key, err := admin.CreateKey(t.Context(), alice, "usage", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	apiUsage := func() limits.Snapshot {
		t.Helper()
		resp := f.call(t, key, "GET", "/v1/me/usage", "")
		defer resp.Body.Close()
		var result limits.Snapshot
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != 200 {
			t.Fatal("API usage", resp.StatusCode, err)
		}
		return result
	}
	beforeHistory, beforeUsage := history(), apiUsage()
	if err := admin.DeleteUser(t.Context(), forwarder.webui); err != nil {
		t.Fatal(err)
	}
	afterHistory, afterUsage := history(), apiUsage()
	if !reflect.DeepEqual(beforeHistory, afterHistory) || !reflect.DeepEqual(beforeUsage.Models, afterUsage.Models) || beforeUsage.Used.Today != afterUsage.Used.Today || beforeUsage.Used.Month != afterUsage.Used.Month {
		t.Fatal("erasing forwarding service changed the end-user portal or usage API", beforeHistory, afterHistory, beforeUsage, afterUsage)
	}
}

func TestShutdownPersistsQueuedUsageWithUnavailableCollector(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	releaseCollector, exporting := make(chan struct{}), make(chan struct{})
	var firstExport sync.Once
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstExport.Do(func() { close(exporting) })
		select {
		case <-releaseCollector:
		case <-r.Context().Done():
		}
	}))
	defer collector.Close()
	defer close(releaseCollector)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", collector.URL+"/v1/traces")
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	buildCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(buildCtx, "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	backend := httptest.NewServer(mockllm.New(mockllm.Options{Tokens: 7}))
	defer backend.Close()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, fmt.Appendf(nil, `models:
  chat-model:
    protocols: [openai]
    timeout_s: 5
    pool: [{url: %s, weight: 1, max_inflight: 4}]
groups:
  staff: {models: [chat-model]}
`, backend.URL), 0600); err != nil {
		t.Fatal(err)
	}
	admin := auth.Admin{Pool: pool}
	owner, err := admin.AddUser(t.Context(), "shutdown", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := admin.CreateKey(t.Context(), owner, "shutdown", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base, stop := startRouter(t, binary, configPath, database.dsn)
	// Keep the completed request queued/in progress until shutdown starts.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `LOCK TABLE usage IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", base+"/v1/chat/completions", strings.NewReader(strings.Replace(limitedBody, `"alias"`, `"chat-model"`, 1)))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	waitFor(t, func() bool {
		var blocked int
		err := pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'INSERT INTO usage%'`).Scan(&blocked)
		return err == nil && blocked == 1
	})
	started, stopped := time.Now(), make(chan struct{})
	go func() { stop(); close(stopped) }()
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		var rows int
		err := pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE user_id=$1 AND input_tokens+output_tokens=10`, owner).Scan(&rows)
		return err == nil && rows == 1
	})
	if time.Since(started) >= 5*time.Second {
		t.Fatal("collector delayed accounting beyond its flush budget")
	}
	select {
	case <-exporting:
	case <-time.After(5 * time.Second):
		t.Fatal("collector was not exercised")
	}
	// The collector remains unresponsive through its own five-second deadline.
	select {
	case <-stopped:
	case <-time.After(8 * time.Second):
		t.Fatal("collector prevented bounded shutdown")
	}
	var tokens int64
	if err := pool.QueryRow(t.Context(), `SELECT sum(input_tokens+output_tokens) FROM usage_daily WHERE user_id=$1`, owner).Scan(&tokens); err != nil || tokens != 10 {
		t.Fatal("missing shutdown charge", tokens, err)
	}
}
