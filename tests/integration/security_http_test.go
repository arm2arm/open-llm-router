package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/apitoken"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestAgentHTTPAuthorizationAndPreviewBoundaries(t *testing.T) {
	f := startPortal(t, usageOptions{portalConfig: func(cfg *config.Config) { cfg.AdminAPI.Enabled = true }})
	admin := signInLocalAdmin(t, f)
	var root string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM users WHERE protected_admin`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	a := auth.Admin{Pool: f.pool}
	other, err := a.CreateLocalUser(t.Context(), "http-boundary-user", "", []string{"staff"}, []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	issue := func() (adminaccess.Key, string) {
		k, raw, err := adminaccess.Issue(t.Context(), f.pool, root, adminaccess.Create{Label: "HTTP boundary", Days: 1, Permissions: adminaccess.Permissions, Networks: []string{"127.0.0.1/32"}}, nil, 20, audit.Actor{Type: "portal_user", ID: root})
		if err != nil {
			t.Fatal(err)
		}
		return k, raw
	}
	_, raw := issue()
	_, secondRaw := issue()
	call := func(raw, path string, body any, want int) []byte {
		t.Helper()
		data, _ := json.Marshal(body)
		r, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+path, strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer "+raw)
		r.Header.Set("Content-Type", "application/json")
		response, err := f.router.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		out, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s status %d want %d", path, response.StatusCode, want)
		}
		return out
	}
	args := map[string]any{"parameters": map[string]string{"id": other}, "body": map[string]any{"enabled": false}}
	call(raw, "/api/admin/v1/read/set_user_enabled", args, 404)
	preview := func() string {
		var result struct{ ID string }
		data := call(raw, "/api/admin/v1/changes/preview", map[string]any{"operation": "set_user_enabled", "arguments": args}, 201)
		if err := json.Unmarshal(data, &result); err != nil || result.ID == "" {
			t.Fatal("preview missing", err)
		}
		return result.ID
	}
	id := preview()
	body := map[string]any{"arguments": args, "idempotency_key": "boundary-apply"}
	call(secondRaw, "/api/admin/v1/changes/"+id+"/apply", body, 404)
	// A persisted preview from an older key policy must independently fail,
	// even if the global administration state has not otherwise changed.
	execSQL(t, f.pool, `UPDATE admin_changes SET policy_version=policy_version-1 WHERE id=$1`, id)
	call(raw, "/api/admin/v1/changes/"+id+"/apply", body, 409)
	var enabled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT enabled FROM users WHERE id=$1`, other).Scan(&enabled); err != nil || !enabled {
		t.Fatal("refused change executed", err)
	}
	ordinary := f.browser(t)
	ordinaryID := ordinary.login(t, "ordinary-agent-key-user", []string{"staff"})
	ordinaryKey, _, err := adminaccess.Issue(t.Context(), f.pool, ordinaryID, adminaccess.Create{Label: "previous administrator key", Days: 1, Permissions: []string{"service:read"}, Networks: []string{"127.0.0.1/32"}}, nil, 20, audit.Actor{Type: "portal_user", ID: ordinaryID})
	if err != nil {
		t.Fatal(err)
	}
	narrowBody, _ := json.Marshal(map[string]any{"permissions": ordinaryKey.Permissions, "allowed_networks": ordinaryKey.Networks, "expires": ordinaryKey.Expires})
	for _, route := range []struct{ method, suffix, body string }{
		{"GET", "", ""},
		{"POST", "", `{"label":"foreign","expires_in_days":1,"permissions":["service:read"],"allowed_networks":["127.0.0.1"]}`},
		{"PUT", "/" + ordinaryKey.ID, string(narrowBody)},
		{"DELETE", "/00000000-0000-0000-0000-000000000001", ""},
	} {
		r, _ := ordinary.call(t, route.method, "/portal/api/admin/agent-keys"+route.suffix, route.body, f.cfg.Portal.BaseURL)
		if r.StatusCode != 403 {
			t.Fatalf("non-admin %s agent keys status %d", route.method, r.StatusCode)
		}
	}
	// A valid JSON object below the ordinary limit remains accepted. A valid
	// object padded past that limit must be rejected before any operation.
	response, _ := admin.call(t, "POST", "/portal/api/admin/agent-keys", strings.Repeat(" ", 9000)+`{"label":"bounded","expires_in_days":1,"permissions":["service:read"],"allowed_networks":["127.0.0.1"]}`, f.cfg.Portal.BaseURL)
	if response.StatusCode != 400 {
		t.Fatal("oversized portal JSON was not bounded", response.StatusCode)
	}
}

func TestDisabledAgentAPIRefusesValidAdministrationKey(t *testing.T) {
	f := startPortal(t)
	_ = signInLocalAdmin(t, f)
	var root string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM users WHERE protected_admin`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	_, raw, err := adminaccess.Issue(t.Context(), f.pool, root, adminaccess.Create{Label: "disabled API", Days: 1, Permissions: []string{"service:read"}, Networks: []string{"127.0.0.1/32"}}, nil, 10, audit.Actor{Type: "portal_user", ID: root})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/api/admin/v1/identity", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	response, err := f.router.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("disabled administration API authenticated", response.StatusCode)
	}
}

func TestAccessTokenCacheCannotOutliveToken(t *testing.T) {
	f := startTokenRouter(t)
	expires := time.Now().Add(2 * time.Second).Truncate(time.Second)
	token := f.token(t, func(c map[string]any) { c["exp"] = expires.Unix() })
	// A stable generation isolates expiry from unrelated identity notifications.
	verifier, err := apitoken.New(t.Context(), f.oidc, f.pool, func() (uint64, bool) { return 1, true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer verifier.Close()
	if _, err := verifier.Authenticate(t.Context(), token); err != nil {
		t.Fatal("valid short token rejected", err)
	}
	if _, err := verifier.Authenticate(t.Context(), token); err != nil {
		t.Fatal("cached short token rejected", err)
	}
	time.Sleep(time.Until(expires.Add(100 * time.Millisecond)))
	if _, err := verifier.Authenticate(t.Context(), token); err != auth.ErrInvalidKey {
		t.Fatal("expired token stayed cached", err)
	}
}

func TestDuplicateAuthorizationCannotHideBesideValidKey(t *testing.T) {
	f := startPortal(t)
	for _, header := range []string{"Authorization", "X-Api-Key"} {
		r, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
		if header == "Authorization" {
			r.Header.Set("X-Api-Key", f.key)
		} else {
			r.Header.Set("Authorization", "Bearer "+f.key)
		}
		r.Header.Add(header, "attacker-header-one")
		r.Header.Add(header, "attacker-header-two")
		response, err := f.router.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Fatal("ambiguous authorization accepted", header, response.StatusCode)
		}
	}
}

func TestPortalSessionIssuerIsCheckedWithoutCleanup(t *testing.T) {
	f := startPortal(t)
	browser := f.browser(t)
	id := browser.login(t, "issuer-boundary", []string{"staff"})
	execSQL(t, f.pool, `UPDATE users SET issuer='https://previous-issuer.example' WHERE id=$1`, id)
	response, _ := browser.call(t, "GET", "/portal/api/me", "", "")
	if response.StatusCode != 401 {
		t.Fatal("session from another issuer authenticated", response.StatusCode)
	}
}
