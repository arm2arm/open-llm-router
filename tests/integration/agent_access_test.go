package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAgentAdministrationLifecycle(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
	}})
	browser := signInLocalAdmin(t, f)
	issue := func(permissions, networks []string) string {
		body, _ := json.Marshal(map[string]any{"label": "test agent", "permissions": permissions, "allowed_networks": networks, "expires_in_days": 1})
		response, data := browser.call(t, "POST", "/portal/api/admin/agent-keys", string(body), f.cfg.Portal.BaseURL)
		if response.StatusCode != 201 {
			t.Fatalf("issue status %d", response.StatusCode)
		}
		var out struct{ Key string }
		if json.Unmarshal(data, &out) != nil || out.Key == "" {
			t.Fatal("missing one-time key")
		}
		return out.Key
	}
	key := issue(adminaccess.Permissions, []string{"127.0.0.1"})
	call := func(key, method, path string, body any, want int) []byte {
		encoded, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+path, strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		// A forged forwarded address must not defeat the socket-peer allowlist.
		req.Header.Set("X-Forwarded-For", "198.51.100.90")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s %s status %d want %d (body omitted to protect one-time credentials)", method, path, response.StatusCode, want)
		}
		return data
	}
	call(key, "GET", "/api/admin/v1/identity", nil, 200)
	if python := os.Getenv("OLR_TEST_MCP_PYTHON"); python != "" {
		script, err := filepath.Abs("../clients/check_admin_mcp.py")
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), python, script)
		command.Env = append(os.Environ(), "OLR_ADMIN_BASE_URL="+f.router.URL, "OLR_ADMIN_API_KEY="+key, "OLR_TEST_MCP_WRITE=1", "OLR_TEST_MCP_USER_ID="+f.owner)
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("MCP SDK check failed: %v %s", err, out)
		}
	}

	call(key, "GET", "/v1/models", nil, 401)
	call(f.key, "GET", "/api/admin/v1/identity", nil, 401)
	call("olr-not-an-admin-key", "GET", "/api/admin/v1/identity", nil, 401)
	denied := issue([]string{"service:read"}, []string{"192.0.2.1"})
	call(denied, "GET", "/api/admin/v1/identity", nil, 403)
	observer := issue([]string{"service:read"}, []string{"127.0.0.1"})
	call(observer, "POST", "/api/admin/v1/read/list_users", map[string]any{}, 403)
	call(observer, "POST", "/api/admin/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, 200)
	// Both setting and clearing an override use the normal audited domain service.
	override := map[string]any{"parameters": map[string]string{"id": f.owner, "field": "tokens_per_day"}, "body": map[string]any{"value": 123, "reason": "agent acceptance"}}
	var overrideChange struct{ ID string }
	data := call(key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "set_user_override", "arguments": override}, 201)
	_ = json.Unmarshal(data, &overrideChange)
	call(key, "POST", "/api/admin/v1/changes/"+overrideChange.ID+"/apply", map[string]any{"arguments": override, "idempotency_key": "set-override-once"}, 204)
	delete(override, "body")
	data = call(key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "clear_user_override", "arguments": override}, 201)
	_ = json.Unmarshal(data, &overrideChange)
	call(key, "POST", "/api/admin/v1/changes/"+overrideChange.ID+"/apply", map[string]any{"arguments": override, "idempotency_key": "clear-override-once"}, 204)
	var actorDetails string
	if err := f.pool.QueryRow(t.Context(), `SELECT details->'agent' FROM audit_events WHERE details->'agent'->>'change_id'=$1 AND action='user.override_clear' ORDER BY id DESC LIMIT 1`, overrideChange.ID).Scan(&actorDetails); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"type": "admin_api_key"`, `"source_ip": "127.0.0.1"`, `"transport": "rest"`, overrideChange.ID} {
		if !strings.Contains(actorDetails, expected) {
			t.Fatal("mutation audit lost agent attribution")
		}
	}
	args := map[string]any{"body": map[string]any{"username": "agent-created-user", "password": "agent-temp-private-1234", "groups": []string{"staff"}}}
	preview := call(key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "create_user", "arguments": args}, 201)
	if strings.Contains(string(preview), "agent-temp-private") {
		t.Fatal("preview exposed password")
	}
	var change struct{ ID string }
	_ = json.Unmarshal(preview, &change)
	call(key, "POST", "/api/admin/v1/changes/"+change.ID+"/apply", map[string]any{"arguments": args, "idempotency_key": "create-user-one"}, 201)
	// Replay returns metadata, never executes the creation again.
	call(key, "POST", "/api/admin/v1/changes/"+change.ID+"/apply", map[string]any{"arguments": args, "idempotency_key": "create-user-one"}, 200)
	call(key, "GET", "/api/admin/v1/changes/"+change.ID, nil, 200)
	call(key, "POST", "/api/admin/v1/read/list_users", map[string]any{}, 200)
	preview = call(key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "create_user", "arguments": args}, 201)
	_ = json.Unmarshal(preview, &change)
	changed := map[string]any{"body": map[string]any{"username": "tampered", "password": "agent-temp-private-1234", "groups": []string{"staff"}}}
	call(key, "POST", "/api/admin/v1/changes/"+change.ID+"/apply", map[string]any{"arguments": changed, "idempotency_key": "tamper-one"}, 409)
	// A browser-issued key changes relevant state and invalidates an older preview.
	_ = issue([]string{"service:read"}, []string{"127.0.0.1"})
	call(key, "POST", "/api/admin/v1/changes/"+change.ID+"/apply", map[string]any{"arguments": args, "idempotency_key": "stale-one"}, 409)
	// Expired previews and uncertain claims must never execute a fresh write.
	preview = call(key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "create_user", "arguments": changed}, 201)
	_ = json.Unmarshal(preview, &change)
	if _, err := f.pool.Exec(t.Context(), `UPDATE admin_changes SET expires=clock_timestamp()-interval '1 second' WHERE id=$1`, change.ID); err != nil {
		t.Fatal(err)
	}
	call(key, "POST", "/api/admin/v1/changes/"+change.ID+"/apply", map[string]any{"arguments": changed, "idempotency_key": "expired-one"}, 409)
	if _, err := f.pool.Exec(t.Context(), `UPDATE admin_changes SET status='running',idempotency_key='uncertain-one' WHERE id=$1`, change.ID); err != nil {
		t.Fatal(err)
	}
	status := call(key, "POST", "/api/admin/v1/changes/"+change.ID+"/apply", map[string]any{"arguments": changed, "idempotency_key": "uncertain-one"}, 200)
	if !strings.Contains(string(status), "outcome_uncertain") {
		t.Fatal("uncertain operation did not require target inspection")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE username='tampered'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("uncertain operation was repeated", err)
	}
	var persisted string
	if err := f.pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(summary::text,''),'') FROM admin_changes`).Scan(&persisted); err != nil || strings.Contains(persisted, "agent-temp-private") {
		t.Fatal("persisted preview leaked a password", err)
	}
	metadata := call(key, "GET", "/api/admin/v1/identity", nil, 200)
	var identity struct {
		KeyID string `json:"key_id"`
	}
	_ = json.Unmarshal(metadata, &identity)
	resp, _ := browser.call(t, "DELETE", "/portal/api/admin/agent-keys/"+identity.KeyID, `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 204 {
		t.Fatal("revoke failed")
	}
	call(key, "GET", "/api/admin/v1/identity", nil, 401)
}

func TestAgentTrustedProxyBoundary(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
		cfg.AdminAPI.TrustedProxyCIDRs = []string{"127.0.0.1/32", "10.0.0.0/24"}
	}})
	browser := signInLocalAdmin(t, f)
	body := `{"label":"proxy test","permissions":["service:read"],"allowed_networks":["198.51.100.9"],"expires_in_days":1}`
	response, data := browser.call(t, "POST", "/portal/api/admin/agent-keys", body, f.cfg.Portal.BaseURL)
	if response.StatusCode != 201 {
		t.Fatal("issue failed")
	}
	var key struct{ Key string }
	_ = json.Unmarshal(data, &key)
	for _, tc := range []struct {
		chain string
		want  int
	}{{"198.51.100.9, 10.0.0.2", 200}, {"203.0.113.4, 198.51.100.9", 200}, {"198.51.100.9, 203.0.113.4", 403}, {"", 403}, {"invalid, 198.51.100.9", 403}} {
		req, err := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/api/admin/v1/identity", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key.Key)
		if tc.chain != "" {
			req.Header.Set("X-Forwarded-For", tc.chain)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != tc.want {
			t.Fatalf("proxy boundary status %d want %d", response.StatusCode, tc.want)
		}
	}
}

func TestAgentRecentGrantAndOwnerRevocation(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
	}})
	browser := f.browser(t)
	browser.login(t, "sso-agent-admin", []string{"admins"})
	issue := func(permissions []string, want int) string {
		body, _ := json.Marshal(map[string]any{"label": "SSO agent", "permissions": permissions, "allowed_networks": []string{"127.0.0.1"}, "expires_in_days": 1})
		response, data := browser.call(t, "POST", "/portal/api/admin/agent-keys", string(body), f.cfg.Portal.BaseURL)
		if response.StatusCode != want {
			t.Fatalf("issue status %d want %d", response.StatusCode, want)
		}
		var out struct{ Key string }
		_ = json.Unmarshal(data, &out)
		return out.Key
	}
	for _, permission := range []string{"users:credentials", "authentication:write", "admin_keys:delegate"} {
		issue([]string{permission}, 403)
	}
	key := issue([]string{"users:read", "users:write"}, 201)
	var owner string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM users WHERE sub='sso-agent-admin'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	request := func(path, method, body string, want int) {
		req, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s status %d want %d", path, resp.StatusCode, want)
		}
	}
	request("/api/admin/v1/identity", "GET", "", 200)
	// User-write alone cannot grant administrator membership or reset credentials.
	request("/api/admin/v1/changes/preview", "POST", `{"operation":"set_user_groups","arguments":{"parameters":{"id":"`+f.owner+`"},"body":{"groups":["admins"]}}}`, 403)
	request("/api/admin/v1/changes/preview", "POST", `{"operation":"reset_user_password","arguments":{"parameters":{"id":"`+f.owner+`"},"body":{"password":"private-reset-attempt"}}}`, 403)
	if _, err := f.pool.Exec(t.Context(), `UPDATE sessions SET created=clock_timestamp()-interval '16 minutes' WHERE user_id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	issue([]string{"service:read"}, 403)
	// Membership changes permanently invalidate keys, even if admin is restored.
	for _, groups := range [][]string{{"staff"}, {"admins"}} {
		if _, err := f.pool.Exec(t.Context(), `UPDATE users SET groups=$2 WHERE id=$1`, owner, groups); err != nil {
			t.Fatal(err)
		}
		request("/api/admin/v1/identity", "GET", "", 401)
	}
}
