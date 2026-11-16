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

func startForwarder(t *testing.T, unknown string, mappings ...map[string]string) *forwarderSetup {
	t.Helper()
	headers := map[string]string{"X-Olr-End-User": "username", "X-Openwebui-User-Email": "email"}
	if len(mappings) > 0 {
		headers = mappings[0]
	}
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
		forwarders: &config.Forwarders{UnknownUser: unknown, HeaderMap: headers},
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

func TestProtectedAdministratorCannotBeForwarded(t *testing.T) {
	for _, unknown := range []string{config.UnknownUserReject, config.UnknownForwarderPolicy} {
		t.Run(unknown, func(t *testing.T) {
			f := startForwarder(t, unknown, map[string]string{
				"A-End-User": "username", "X-Olr-End-User": "username",
				"X-Openwebui-User-Email": "email", "Z-End-User": "id",
			})
			admin := auth.Admin{Pool: f.pool}
			root, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
			if err != nil {
				t.Fatal(err)
			}
			if err := admin.ChangeLocalPassword(t.Context(), root.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
				t.Fatal(err)
			}
			execSQL(t, f.pool, `UPDATE users SET email='root@example.org' WHERE id=$1`, root.ID)
			for name, headers := range map[string]map[string]string{
				"username":      {"X-OLR-End-User": "admin"},
				"email":         {"X-OpenWebUI-User-Email": "ROOT@example.org"},
				"id":            {"Z-End-User": root.ID},
				"unknown first": {"A-End-User": "unknown", "X-OLR-End-User": "admin"},
				"unknown last":  {"X-OLR-End-User": "admin", "X-OpenWebUI-User-Email": "unknown@example.org"},
			} {
				t.Run(name, func(t *testing.T) {
					for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
						if resp := f.send(t, f.key, headers, path); resp.StatusCode != http.StatusForbidden {
							t.Fatal("protected administrator was accepted", resp.StatusCode)
						}
					}
				})
			}
			// A protected match must not be hidden behind LIMIT 2 or ambiguity.
			for _, sub := range []string{"duplicate-one", "duplicate-two", "duplicate-three"} {
				id := user(t, f.pool, "https://idp.example/realm", sub)
				execSQL(t, f.pool, `UPDATE users SET username='admin',email='root@example.org' WHERE id=$1`, id)
			}
			for _, headers := range []map[string]string{{"X-OLR-End-User": "admin"}, {"X-OpenWebUI-User-Email": "root@example.org"}} {
				if resp := f.send(t, f.key, headers); resp.StatusCode != http.StatusForbidden {
					t.Fatal("ambiguous protected match was accepted", resp.StatusCode)
				}
			}
			// A normal API key still acts as its own user, ignoring forwarded names.
			if resp := f.send(t, f.usageRouter.key, map[string]string{"X-OLR-End-User": "admin"}); resp.StatusCode != http.StatusOK {
				t.Fatal("ordinary key changed identity", resp.StatusCode)
			}
			if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "alice"}); resp.StatusCode != http.StatusOK {
				t.Fatal("ordinary forwarded user refused", resp.StatusCode)
			}
			rootKey, err := admin.CreateKey(t.Context(), root.ID, "own-access", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if resp := f.send(t, rootKey, nil); resp.StatusCode != http.StatusOK {
				t.Fatal("administrator's own key refused", resp.StatusCode)
			}
			f.stop()
			if f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, root.ID) != 0 ||
				f.rows(t, `SELECT count(*) FROM usage WHERE user_id=$1 AND forwarded_for IS NOT NULL`, root.ID) != 0 ||
				f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, f.alice) != 1 ||
				f.writer.Counters().Written != 3 {
				t.Fatal("unexpected usage attribution", f.writer.Counters())
			}
		})
	}
}

func (f *forwarderSetup) send(t *testing.T, key string, headers map[string]string, paths ...string) *http.Response {
	t.Helper()
	path := "/v1/chat/completions"
	if len(paths) > 0 {
		path = paths[0]
	}
	req, err := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+path, strings.NewReader(`{"model":"chat-model","messages":[]}`))
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

func TestForwarderLookupIgnoresUnavailableDuplicates(t *testing.T) {
	for _, unknown := range []string{config.UnknownUserReject, config.UnknownForwarderPolicy} {
		t.Run(unknown, func(t *testing.T) {
			f := startForwarder(t, unknown)
			admin := auth.Admin{Pool: f.pool}
			var old []string
			for _, sub := range []string{"old-one", "old-two", "old-three"} {
				id := user(t, f.pool, "https://old-idp.example/realm", sub)
				execSQL(t, f.pool, `UPDATE users SET username='shared',email='shared@example.org',groups='{staff}' WHERE id=$1`, id)
				if err := admin.DisableUser(t.Context(), id); err != nil {
					t.Fatal(err)
				}
				old = append(old, id)
			}
			active := user(t, f.pool, "https://current-idp.example/realm", "active")
			execSQL(t, f.pool, `UPDATE users SET username='shared',email='shared@example.org',groups='{staff}' WHERE id=$1`, active)
			headers := []map[string]string{{"X-OLR-End-User": "shared"}, {"X-OpenWebUI-User-Email": "SHARED@example.org"}}
			assertStatus := func(want int) {
				t.Helper()
				for _, h := range headers {
					if resp := f.send(t, f.key, h); resp.StatusCode != want {
						t.Fatalf("%v: got %d, want %d", h, resp.StatusCode, want)
					}
				}
			}
			assertStatus(200)
			assertStatus(200) // Repeat after warming both lookup caches.
			if err := admin.DisableUser(t.Context(), active); err != nil {
				t.Fatal(err)
			}
			assertStatus(403) // Disabled-only matches never use forwarder fallback.
			if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "unknown", "X-OpenWebUI-User-Email": "shared@example.org"}); resp.StatusCode != 403 {
				t.Fatal("unknown header hid a disabled match", resp.StatusCode)
			}
			if err := admin.SetEnabled(t.Context(), old[0], true); err != nil {
				t.Fatal(err)
			}
			assertStatus(200)
			if err := admin.SetEnabled(t.Context(), old[1], true); err != nil {
				t.Fatal(err)
			}
			ambiguousStatus := 403
			if unknown == config.UnknownForwarderPolicy {
				ambiguousStatus = 200
			}
			assertStatus(ambiguousStatus)
			f.stop()
			if f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, active) != 4 ||
				f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, old[0]) != 2 ||
				f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1 OR forwarded_for=$2`, old[1], old[2]) != 0 ||
				f.rows(t, `SELECT count(*) FROM usage_daily WHERE user_id=$1 AND requests=4`, active) != 1 {
				t.Fatal("lookup selected or charged the wrong identity")
			}
			fallbackCalls := 0
			if unknown == config.UnknownForwarderPolicy {
				fallbackCalls = 2
			}
			if f.rows(t, `SELECT count(*) FROM usage WHERE user_id=$1 AND forwarded_for IS NULL`, f.webui) != fallbackCalls ||
				int(f.writer.Counters().Written) != 6+fallbackCalls {
				t.Fatal("unexpected forwarder fallback or duplicate recording", f.writer.Counters())
			}
		})
	}
}

func TestForwarderLookupIgnoresPasswordChangePendingDuplicates(t *testing.T) {
	for _, unknown := range []string{config.UnknownUserReject, config.UnknownForwarderPolicy} {
		t.Run(unknown, func(t *testing.T) {
			f := startForwarder(t, unknown)
			admin := auth.Admin{Pool: f.pool}
			pending, err := admin.CreateLocalUser(t.Context(), "pending", "pending@example.org", []string{"staff"}, []byte(initialAdminPassword))
			if err != nil {
				t.Fatal(err)
			}
			active := user(t, f.pool, "https://idp.example/realm", "active")
			execSQL(t, f.pool, `UPDATE users SET username='pending',email='pending@example.org',groups='{staff}' WHERE id=$1`, active)
			for _, h := range []map[string]string{{"X-OLR-End-User": "pending"}, {"X-OpenWebUI-User-Email": "PENDING@example.org"}} {
				if resp := f.send(t, f.key, h); resp.StatusCode != 200 {
					t.Fatal("pending duplicate hid the enabled account", resp.StatusCode)
				}
			}
			if err := admin.DisableUser(t.Context(), active); err != nil {
				t.Fatal(err)
			}
			if resp := f.send(t, f.key, map[string]string{"X-OLR-End-User": "pending"}); resp.StatusCode != 403 {
				t.Fatal("pending-only match used fallback", resp.StatusCode)
			}
			f.stop()
			if f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, active) != 2 ||
				f.rows(t, `SELECT count(*) FROM usage WHERE forwarded_for=$1`, pending) != 0 || f.writer.Counters().Written != 2 {
				t.Fatal("pending account usage attribution", f.writer.Counters())
			}
		})
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
