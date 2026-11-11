package integration_test

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/policy"
)

type seenHeaders struct {
	mu   sync.Mutex
	list []http.Header
}

func (s *seenHeaders) add(h http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, h.Clone())
}

func (s *seenHeaders) any(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.list {
		if h.Get(name) != "" {
			return true
		}
	}
	return false
}

type forwarderSetup struct {
	*usageRouter
	seen       *seenHeaders
	webui, key string // the forwarder's user and trusted key
	alice      string
}

func startForwarder(t *testing.T, unknown string) *forwarderSetup {
	t.Helper()
	seen := &seenHeaders{}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			seen.add(r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
	})
	f := startUsageRouter(t, backend, usageOptions{
		group:      &config.Group{Models: []string{"*"}, RPM: limitPtr(100)},
		forwarders: &config.Forwarders{UnknownUser: unknown, HeaderMap: map[string]string{"X-Olr-End-User": "username", "X-Openwebui-User-Email": "email"}},
	})
	admin := auth.Admin{Pool: f.pool}
	webui, err := admin.AddUser(t.Context(), "open-webui", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := admin.CreateForwarderKey(t.Context(), webui, "open-webui", "interactive", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := admin.AddUser(t.Context(), "alice", "Alice@Example.org", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	return &forwarderSetup{usageRouter: f, seen: seen, webui: webui, key: key, alice: alice}
}

func (f *forwarderSetup) send(t *testing.T, key string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	for name, value := range headers {
		req.Header.Add(name, value)
	}
	resp, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func (f *forwarderSetup) rows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestForwardedUserIsAttributedAndLimitedAsThemselves(t *testing.T) {
	f := startForwarder(t, config.UnknownUserReject)
	email := map[string]string{"X-OpenWebUI-User-Email": "alice@example.ORG", "X-OpenWebUI-User-Name": "Alice"}
	if resp := f.send(t, f.key, email); resp.StatusCode != 200 {
		t.Fatal("forwarded call", resp.StatusCode)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	if f.rows(t, `SELECT count(*) FROM usage WHERE user_id=$1 AND forwarded_for=$2 AND input_tokens=2`, f.webui, f.alice) != 1 ||
		f.rows(t, `SELECT count(*) FROM usage_daily WHERE user_id=$1 AND requests=1`, f.alice) != 1 ||
		f.rows(t, `SELECT count(*) FROM usage_daily WHERE user_id=$1`, f.webui) != 0 {
		t.Fatal("forwarded call not attributed to the end user")
	}
	// Alice's own limit applies to calls forwarded for her, not the forwarder's.
	if err := (auth.Admin{Pool: f.pool}).SetOverride(t.Context(), f.alice, policy.FieldRPM, limitPtr(int64(1)), "", "test"); err != nil {
		t.Fatal(err)
	}
	requireLimit(t, func() *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model"}`))
		req.Header.Set("Authorization", "Bearer "+f.key)
		req.Header.Set("X-OLR-End-User", "alice")
		resp, err := f.router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}(), "rpm", false)
	if resp := f.send(t, f.key, nil); resp.StatusCode != 200 {
		t.Fatal("the forwarder's own call was limited as alice", resp.StatusCode)
	}
	// The identity headers never reach a backend.
	if f.seen.any("X-OpenWebUI-User-Email") || f.seen.any("X-OLR-End-User") {
		t.Fatal("an end-user header reached the backend")
	}
	if !f.seen.any("X-OpenWebUI-User-Name") {
		t.Fatal("unmapped headers must still be forwarded")
	}
}

func TestEndUserHeaderFromNormalKeyIsIgnored(t *testing.T) {
	f := startForwarder(t, config.UnknownUserReject)
	if resp := f.send(t, f.usageRouter.key, map[string]string{"X-OLR-End-User": "alice", "X-OpenWebUI-User-Email": "alice@example.org"}); resp.StatusCode != 200 {
		t.Fatal("normal key", resp.StatusCode)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	if f.rows(t, `SELECT count(*) FROM usage WHERE user_id=$1 AND forwarded_for IS NULL`, f.owner) != 1 ||
		f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for IS NOT NULL OR user_id=$1`, f.alice) != 0 {
		t.Fatal("a normal key attributed usage to another user")
	}
	if f.seen.any("X-OLR-End-User") || f.seen.any("X-OpenWebUI-User-Email") {
		t.Fatal("ignored identity headers still reached the backend")
	}
}

func TestUnknownDisabledAndConflictingEndUsers(t *testing.T) {
	f := startForwarder(t, config.UnknownUserReject)
	admin := auth.Admin{Pool: f.pool}
	if _, err := admin.AddUser(t.Context(), "alice-2", "alice@example.org", []string{"staff"}); err != nil {
		t.Fatal(err)
	}
	for name, headers := range map[string]map[string]string{
		"unknown":   {"X-OLR-End-User": "mallory"},
		"ambiguous": {"X-OpenWebUI-User-Email": "alice@example.org"}, // two users share it
	} {
		if resp := f.send(t, f.key, headers); resp.StatusCode != 403 {
			t.Fatal(name, resp.StatusCode)
		}
	}
	bob, err := admin.AddUser(t.Context(), "bob", "bob@example.org", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "alice", "X-OpenWebUI-User-Email": "bob@example.org"}); resp.StatusCode != 400 {
		t.Fatal("conflicting headers", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model"}`))
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Add("X-OLR-End-User", "alice")
	req.Header.Add("X-OLR-End-User", "bob")
	if resp, err := f.router.Client().Do(req); err != nil || resp.StatusCode != 400 {
		t.Fatal("repeated header", err)
	}
	// Warm the cache, then disable: the next forwarded call is refused.
	if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "bob"}); resp.StatusCode != 200 {
		t.Fatal("bob", resp.StatusCode)
	}
	if err := admin.DisableUser(t.Context(), bob); err != nil {
		t.Fatal(err)
	}
	if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "bob"}); resp.StatusCode != 403 {
		t.Fatal("disabled end user served", resp.StatusCode)
	}
}

func TestUnknownEndUserFallsBackToForwarderPolicy(t *testing.T) {
	f := startForwarder(t, config.UnknownForwarderPolicy)
	if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "mallory"}); resp.StatusCode != 200 {
		t.Fatal("fallback", resp.StatusCode)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	if f.rows(t, `SELECT count(*) FROM usage WHERE user_id=$1 AND forwarded_for IS NULL`, f.webui) != 1 {
		t.Fatal("fallback call not attributed to the forwarder")
	}
	bob, _ := (auth.Admin{Pool: f.pool}).AddUser(t.Context(), "bob", "", []string{"staff"})
	if err := (auth.Admin{Pool: f.pool}).DisableUser(t.Context(), bob); err != nil {
		t.Fatal(err)
	}
	if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "bob"}); resp.StatusCode != 403 {
		t.Fatal("a disabled user must not fall back to the forwarder", resp.StatusCode)
	}
}
