package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/jackc/pgx/v5/pgxpool"
)

// directorySync is the running group-sync job as the router wires it: it
// syncs groups and looks up users who have not signed in yet.
type directorySync struct{ syncer *groupsync.Syncer }

func (s directorySync) GroupSyncEnabled() bool { return true }
func (s directorySync) SyncGroups(ctx context.Context) (groupsync.Result, error) {
	return s.syncer.RunOnce(ctx)
}
func (s directorySync) LookupIdentity(ctx context.Context, issuer, subject string) (groupsync.Identity, error) {
	if issuer != s.syncer.Issuer() {
		return groupsync.Identity{}, groupsync.ErrDisabled
	}
	return s.syncer.Lookup(ctx, subject)
}

type issuanceFixture struct {
	*portalFixture
	owner string
}

// startIssuance starts a portal whose administration API is enabled. With
// keycloak, missing users are looked up through the group-sync account.
func startIssuance(t *testing.T, keycloak bool, adjust func(*config.Config)) *issuanceFixture {
	t.Helper()
	f := startPortal(t, usageOptions{
		portalConfig: func(cfg *config.Config) { cfg.OIDC.SyncClientID = "olr-sync" },
		portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, _ *proxy.Handler) {
			cfg.AdminAPI.Enabled = true
			if adjust != nil {
				adjust(cfg)
			}
			if !keycloak {
				return
			}
			syncer, err := groupsync.New(t.Context(), cfg.OIDC, "sync-secret", pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			syncer.Stats = &groupsync.Stats{}
			t.Cleanup(syncer.Close)
			h.GroupSync = directorySync{syncer}
		},
	})
	owner := f.browser(t).login(t, "integration-owner", []string{"admins"})
	return &issuanceFixture{portalFixture: f, owner: owner}
}

func (f *issuanceFixture) adminKey(t *testing.T, permissions ...string) (adminaccess.Key, string) {
	t.Helper()
	key, secret, err := adminaccess.Issue(t.Context(), f.pool, f.owner, adminaccess.Create{Label: "integration", Permissions: permissions, Networks: []string{"127.0.0.1/32"}, Days: 1}, nil, 20, audit.Actor{Type: "portal_user", ID: f.owner})
	if err != nil {
		t.Fatal(err)
	}
	return key, secret
}

func (f *issuanceFixture) do(t *testing.T, secret, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	response, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

// change previews an operation and applies it once; it returns the change ID
// and the apply response.
func (f *issuanceFixture) change(t *testing.T, secret, operation string, arguments map[string]any) (string, int, []byte) {
	t.Helper()
	status, data := f.do(t, secret, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": operation, "arguments": arguments})
	if status != 201 {
		t.Fatalf("preview %s: %d %s", operation, status, data)
	}
	var preview struct{ ID string }
	if json.Unmarshal(data, &preview) != nil || preview.ID == "" {
		t.Fatal("preview has no change ID")
	}
	status, data = f.do(t, secret, "POST", "/api/admin/v1/changes/"+preview.ID+"/apply", map[string]any{"arguments": arguments, "idempotency_key": "apply-" + preview.ID})
	return preview.ID, status, data
}

type issuedKey struct {
	Key, ID, Prefix, Label, Class string
	Expires                       time.Time
	BaseURL                       string `json:"base_url"`
	User                          struct {
		Username string
		Created  bool
	}
	Models []struct {
		ID            string
		Protocols     []string
		Type          string
		ContextLength int `json:"context_length"`
	}
	Limits map[string]any
}

func (f *issuanceFixture) issue(t *testing.T, secret string, body map[string]any, want int, wantType string) issuedKey {
	t.Helper()
	if _, ok := body["issuer"]; !ok {
		body["issuer"] = f.provider.issuer
	}
	if _, ok := body["label"]; !ok {
		body["label"] = "REANA"
	}
	_, status, data := f.change(t, secret, "issue_user_key", map[string]any{"body": body})
	if status != want {
		t.Fatalf("issue_user_key status %d want %d: %s", status, want, data)
	}
	var out issuedKey
	if want != 201 {
		var problem struct{ Error struct{ Type string } }
		if json.Unmarshal(data, &problem) != nil || problem.Error.Type != wantType {
			t.Fatalf("issue_user_key error type %q want %q", problem.Error.Type, wantType)
		}
		return out
	}
	if json.Unmarshal(data, &out) != nil || !strings.HasPrefix(out.Key, "olr-") || out.ID == "" || out.Prefix != out.Key[:12] || out.Expires.IsZero() {
		t.Fatal("issued key response lacks the key or its metadata")
	}
	return out
}

func (f *issuanceFixture) keys(t *testing.T, userID string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM api_keys WHERE user_id=$1`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *issuanceFixture) inference(t *testing.T, key, path string) int {
	t.Helper()
	method, body := "GET", ""
	if path != "/v1/models" {
		method, body = "POST", `{"model":"chat-model","messages":[{"role":"user","content":"hi"}],"max_tokens":7}`
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, f.router.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	response, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}

func operationNames(t *testing.T, data []byte) []string {
	t.Helper()
	var out struct{ Operations []struct{ Name string } }
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, op := range out.Operations {
		names = append(names, op.Name)
	}
	return names
}

func TestUserKeyIssuanceDiscoveryIsPermissionFiltered(t *testing.T) {
	f := startIssuance(t, false, nil)
	_, issuer := f.adminKey(t, "user_keys:issue")
	_, reader := f.adminKey(t, "users:read", "users:write", "users:credentials")
	ours := []string{"issue_user_key", "revoke_issued_key", "get_issued_key_summary"}

	status, data := f.do(t, issuer, "GET", "/api/admin/v1/operations", nil)
	names := operationNames(t, data)
	if status != 200 || !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(ours))) {
		t.Fatalf("user_keys:issue alone must discover exactly its operations: %v", names)
	}
	status, data = f.do(t, reader, "GET", "/api/admin/v1/operations", nil)
	names = operationNames(t, data)
	if status != 200 || slices.ContainsFunc(ours, func(name string) bool { return slices.Contains(names, name) }) || !slices.Contains(names, "revoke_user_key") {
		t.Fatalf("user permissions must not imply user_keys:issue: %v", names)
	}

	// The permission does not imply users:read.
	if status, _ := f.do(t, issuer, "POST", "/api/admin/v1/read/list_users", map[string]any{}); status != 403 {
		t.Fatalf("user_keys:issue read users: %d", status)
	}
	if status, _ := f.do(t, reader, "POST", "/api/admin/v1/read/get_issued_key_summary", map[string]any{"parameters": map[string]string{"key_id": "00000000-0000-0000-0000-000000000000"}}); status != 403 {
		t.Fatalf("summary without user_keys:issue: %d", status)
	}
	if status, _ := f.do(t, reader, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "issue_user_key", "arguments": map[string]any{"body": map[string]any{"issuer": f.provider.issuer, "subject": "x", "label": "x"}}}); status != 403 {
		t.Fatalf("preview without user_keys:issue: %d", status)
	}

	status, data = f.do(t, issuer, "GET", "/api/admin/v1/openapi.json", nil)
	var spec struct{ Paths map[string]json.RawMessage }
	if status != 200 || json.Unmarshal(data, &spec) != nil || spec.Paths["/read/get_issued_key_summary"] == nil || spec.Paths["/read/list_users"] != nil {
		t.Fatalf("OpenAPI read paths are not filtered: %d", status)
	}
	if preview := string(spec.Paths["/changes/preview"]); !strings.Contains(preview, `"issue_user_key"`) || !strings.Contains(preview, `"revoke_issued_key"`) || strings.Contains(preview, `"revoke_user_key"`) {
		t.Fatal("OpenAPI change schema is not filtered")
	}
	status, data = f.do(t, reader, "GET", "/api/admin/v1/openapi.json", nil)
	if status != 200 || strings.Contains(string(data), "issue_user_key") || strings.Contains(string(data), "get_issued_key_summary") {
		t.Fatal("OpenAPI offers issuance without its permission")
	}

	tools := func(secret string) string {
		t.Helper()
		status, data := f.do(t, secret, "POST", "/api/admin/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		if status != 200 {
			t.Fatalf("MCP tools/list: %d", status)
		}
		return string(data)
	}
	if listed := tools(issuer); !strings.Contains(listed, `"name":"get_issued_key_summary"`) || !strings.Contains(listed, `"issue_user_key"`) || !strings.Contains(listed, `"name":"preview_change"`) || strings.Contains(listed, `"name":"list_users"`) {
		t.Fatal("MCP discovery for user_keys:issue is not filtered")
	}
	if listed := tools(reader); strings.Contains(listed, "get_issued_key_summary") || strings.Contains(listed, "issue_user_key") {
		t.Fatal("MCP offers issuance without its permission")
	}
	status, data = f.do(t, reader, "POST", "/api/admin/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "get_issued_key_summary", "arguments": map[string]any{}}})
	if status != 200 || !strings.Contains(string(data), "tool unavailable") {
		t.Fatal("MCP executed a filtered tool")
	}
}

func TestUserKeyIssuanceForExistingAndKeycloakUsers(t *testing.T) {
	f := startIssuance(t, true, nil)
	issuerKey, secret := f.adminKey(t, "user_keys:issue")
	_, other := f.adminKey(t, "user_keys:issue")
	browser := f.browser(t)
	existing := browser.login(t, "existing-user", []string{"staff"})

	// An existing SSO user.
	key := f.issue(t, secret, map[string]any{"subject": "existing-user", "label": "  REANA  ", "class": "batch"}, 201, "")
	if key.Label != "Integration · REANA" || key.Class != "batch" || key.User.Username != "existing-user" || key.User.Created || key.BaseURL != f.cfg.Portal.BaseURL+"/v1" {
		t.Fatalf("issued key metadata: %+v", key)
	}
	var models []string
	for _, m := range key.Models {
		models = append(models, m.ID)
		if m.ID == "chat-model" && !slices.Equal(m.Protocols, []string{"openai", "anthropic"}) {
			t.Fatal("model protocols missing")
		}
		// The same metadata as GET /v1/models: mock-llm's max_model_len, read by
		// the health check, also for the alias.
		if m.Type != "chat" || m.ContextLength != 32768 {
			t.Fatalf("%s: type %q context_length %d", m.ID, m.Type, m.ContextLength)
		}
	}
	if !slices.Equal(models, []string{"alias", "chat-model"}) || key.Limits == nil {
		t.Fatalf("models or limits: %v %v", models, key.Limits)
	}
	if lifetime := time.Until(key.Expires); lifetime < 89*24*time.Hour || lifetime > 90*24*time.Hour {
		t.Fatalf("default lifetime %s", lifetime)
	}
	if f.inference(t, key.Key, "/v1/chat/completions") != 200 {
		t.Fatal("issued key cannot call the router")
	}
	var issuedBy string
	if err := f.pool.QueryRow(t.Context(), `SELECT issued_by_admin_key::text FROM api_keys WHERE id=$1 AND user_id=$2`, key.ID, existing).Scan(&issuedBy); err != nil || issuedBy != issuerKey.ID {
		t.Fatal("key does not record its issuing administration key", err)
	}
	// The user sees and controls the key in the portal under its label.
	_, data := browser.call(t, "GET", "/portal/api/keys", "", "")
	if !strings.Contains(string(data), `"label":"Integration · REANA"`) || !strings.Contains(string(data), key.ID) {
		t.Fatal("issued key missing from the user's portal key list")
	}

	// Replaying an applied change returns its status, never a second key.
	status, data := f.do(t, secret, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "issue_user_key", "arguments": map[string]any{"body": map[string]any{"issuer": f.provider.issuer, "subject": "existing-user", "label": "replay"}}})
	var preview struct{ ID string }
	if status != 201 || json.Unmarshal(data, &preview) != nil {
		t.Fatal("preview failed")
	}
	if f.keys(t, existing) != 1 {
		t.Fatal("preview issued a key")
	}
	apply := map[string]any{"arguments": map[string]any{"body": map[string]any{"issuer": f.provider.issuer, "subject": "existing-user", "label": "replay"}}, "idempotency_key": "replay-once"}
	if status, _ := f.do(t, secret, "POST", "/api/admin/v1/changes/"+preview.ID+"/apply", apply); status != 201 {
		t.Fatalf("apply %d", status)
	}
	status, data = f.do(t, secret, "POST", "/api/admin/v1/changes/"+preview.ID+"/apply", apply)
	if status != 200 || strings.Contains(string(data), "olr-") || !strings.Contains(string(data), `"secret_recoverable":false`) || f.keys(t, existing) != 2 {
		t.Fatalf("replay repeated issuance or exposed a key: %d", status)
	}

	// A user who never signed in is recorded from Keycloak first.
	f.provider.setUser("kc-new-sub", remoteUser{enabled: true, groups: []string{"students"}, username: "kc-new-user", email: "kc-new@example.test"})
	created := f.issue(t, secret, map[string]any{"subject": "kc-new-sub"}, 201, "")
	if created.User.Username != "kc-new-user" || !created.User.Created || len(created.Models) != 2 || created.Models[1].ID != "chat-model" {
		t.Fatalf("Keycloak user: %+v", created)
	}
	var kcUser, username, email string
	var groups []string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text,username,email,groups FROM users WHERE issuer=$1 AND sub='kc-new-sub' AND enabled`, f.provider.issuer).Scan(&kcUser, &username, &email, &groups); err != nil || username != "kc-new-user" || email != "kc-new@example.test" || !slices.Equal(groups, []string{"students"}) {
		t.Fatal("Keycloak user was not recorded", username, email, groups, err)
	}
	if f.inference(t, created.Key, "/v1/models") != 200 {
		t.Fatal("key for a Keycloak-created user does not work")
	}
	// The second key for the same subject reuses the stored user.
	if again := f.issue(t, secret, map[string]any{"subject": "kc-new-sub"}, 201, ""); again.User.Created {
		t.Fatal("stored user was created again")
	}

	userCount := func() int {
		var n int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := userCount()
	f.issue(t, secret, map[string]any{"subject": "kc-missing"}, 404, "user_not_found")
	// Only the exact user representation for the subject counts.
	f.provider.setUser("kc-alias", remoteUser{enabled: true, groups: []string{"staff"}, id: "kc-someone-else"})
	f.issue(t, secret, map[string]any{"subject": "kc-alias"}, 404, "user_not_found")
	f.provider.setUser("kc-disabled", remoteUser{enabled: false, groups: []string{"staff"}})
	f.issue(t, secret, map[string]any{"subject": "kc-disabled"}, 403, "user_disabled")
	f.provider.setUser("kc-no-models", remoteUser{enabled: true, groups: []string{"research"}})
	f.issue(t, secret, map[string]any{"subject": "kc-no-models"}, 409, "no_model_access")
	if userCount() != before {
		t.Fatal("a refused Keycloak user was recorded")
	}

	// Stored users: no models, disabled.
	noModels := f.browser(t).login(t, "no-models", []string{"research"})
	f.issue(t, secret, map[string]any{"subject": "no-models"}, 409, "no_model_access")
	disabled := f.browser(t).login(t, "disabled-user", []string{"staff"})
	if err := (auth.Admin{Pool: f.pool}).SetEnabled(t.Context(), disabled, false); err != nil {
		t.Fatal(err)
	}
	f.issue(t, secret, map[string]any{"subject": "disabled-user"}, 403, "user_disabled")
	if f.keys(t, noModels) != 0 || f.keys(t, disabled) != 0 {
		t.Fatal("a refused user received a key")
	}

	// Never local accounts or the protected administrator.
	admin := auth.Admin{Pool: f.pool}
	if _, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword)); err != nil {
		t.Fatal(err)
	}
	local, err := admin.AddUser(t.Context(), "local-person", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"admin", "local-person"} {
		f.issue(t, secret, map[string]any{"issuer": "local", "subject": subject}, 403, "issuer_not_allowed")
		f.issue(t, secret, map[string]any{"subject": subject}, 404, "user_not_found")
	}
	f.issue(t, secret, map[string]any{"issuer": "https://other.example/realms/test", "subject": "existing-user"}, 403, "issuer_not_allowed")
	var protected string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM users WHERE protected_admin`).Scan(&protected); err != nil {
		t.Fatal(err)
	}
	if f.keys(t, local) != 0 || f.keys(t, protected) != 0 {
		t.Fatal("a local account received an issued key")
	}

	// Structural and lifetime validation.
	f.issue(t, secret, map[string]any{"subject": "existing-user", "expires_in_days": 91}, 400, "invalid_request_error")
	f.issue(t, secret, map[string]any{"subject": "existing-user", "expires_in_days": 0}, 400, "invalid_request_error")
	f.issue(t, secret, map[string]any{"subject": "existing-user", "label": strings.Repeat("x", 100)}, 400, "invalid_request_error")
	f.issue(t, secret, map[string]any{"subject": ".."}, 400, "invalid_request_error")
	if status, _ := f.do(t, secret, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "issue_user_key", "arguments": map[string]any{"body": map[string]any{"issuer": f.provider.issuer, "subject": "x", "label": "x", "class": "bulk"}}}); status != 400 {
		t.Fatal("unknown class passed structural validation")
	}

	// Summary and revocation are limited to the issuing administration key.
	summary := func(secret, id string) (int, map[string]any) {
		t.Helper()
		status, data := f.do(t, secret, "POST", "/api/admin/v1/read/get_issued_key_summary", map[string]any{"parameters": map[string]string{"key_id": id}})
		if strings.Contains(string(data), key.Key) {
			t.Fatal("summary exposed the key")
		}
		out := map[string]any{}
		_ = json.Unmarshal(data, &out)
		return status, out
	}
	// Quota accounting completes before the durable record is written, so
	// today's usage covers the completion made with the issued key above.
	waitFor(t, func() bool { return f.writer.Counters().Written >= 1 })
	status, view := summary(secret, key.ID)
	user, _ := view["user"].(map[string]any)
	usage, _ := view["usage"].(map[string]any)
	used, _ := usage["used"].(map[string]any)
	if tokens, _ := used["tokens_today"].(float64); tokens <= 0 {
		t.Fatalf("summary lacks today's usage: %v", usage)
	}
	if status != 200 || view["revoked"] != false || view["active"] != true || view["last_used"] == nil || view["expires"] == nil || view["limits"] == nil || view["label"] != "Integration · REANA" || user["username"] != "existing-user" {
		t.Fatalf("summary: %d %v", status, view)
	}
	if models, _ := view["models"].([]any); len(models) != 2 {
		t.Fatal("summary lacks the user's models")
	} else if first, _ := models[0].(map[string]any); first["context_length"] != float64(32768) || first["type"] != "chat" || first["protocols"] == nil {
		t.Fatalf("summary model metadata: %v", first)
	}
	if status, _ := summary(other, key.ID); status != 404 {
		t.Fatalf("another administration key read the summary: %d", status)
	}
	own, err := admin.CreateKey(t.Context(), existing, "personal", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var ownID string
	if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM api_keys WHERE user_id=$1 AND label='personal'`, existing).Scan(&ownID); err != nil {
		t.Fatal(err)
	}
	if status, _ := summary(secret, ownID); status != 404 {
		t.Fatal("summary reached a key the integration did not issue")
	}
	revoke := func(secret, id string) int {
		t.Helper()
		_, status, _ := f.change(t, secret, "revoke_issued_key", map[string]any{"parameters": map[string]string{"key_id": id}})
		return status
	}
	if revoke(other, key.ID) != 404 || revoke(secret, ownID) != 404 {
		t.Fatal("revocation reached a key the caller did not issue")
	}
	if f.inference(t, key.Key, "/v1/models") != 200 || f.inference(t, own, "/v1/models") != 200 {
		t.Fatal("a refused revocation changed a key")
	}
	if revoke(secret, key.ID) != 204 {
		t.Fatal("issuer could not revoke its key")
	}
	if f.inference(t, key.Key, "/v1/models") != 401 {
		t.Fatal("revoked key still authenticates")
	}
	if status, view := summary(secret, key.ID); status != 200 || view["revoked"] != true || view["active"] != false {
		t.Fatal("summary does not report revocation")
	}
	status, data = f.do(t, secret, "POST", "/api/admin/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "get_issued_key_summary", "arguments": map[string]any{"parameters": map[string]string{"key_id": created.ID}}}})
	if status != 200 || !strings.Contains(string(data), `"isError":false`) || !strings.Contains(string(data), "kc-new-user") {
		t.Fatal("MCP summary failed")
	}

	// History names the administration key, target user and key; never a secret.
	for _, check := range []struct{ action, key, user string }{{"user_key.issue", key.ID, existing}, {"user_key.issue", created.ID, kcUser}, {"user_key.revoke", key.ID, existing}} {
		var details string
		if err := f.pool.QueryRow(t.Context(), `SELECT details::text FROM audit_events WHERE action=$1 AND target_type='key' AND target_id=$2 AND result='succeeded'`, check.action, check.key).Scan(&details); err != nil {
			t.Fatal("missing history", check.action, err)
		}
		for _, expected := range []string{`"issued_by_admin_key": "` + issuerKey.ID + `"`, `"key_id": "` + issuerKey.ID + `"`, `"user_id": "` + check.user} {
			if !strings.Contains(details, expected) {
				t.Fatalf("%s history lacks %s", check.action, expected)
			}
		}
	}
	var sync string
	if err := f.pool.QueryRow(t.Context(), `SELECT details->'agent'->>'key_id' FROM audit_events WHERE action='user.identity_sync' AND target_id=$1 AND result='succeeded'`, kcUser).Scan(&sync); err != nil || sync != issuerKey.ID {
		t.Fatal("Keycloak user creation is not attributed to the administration key", err)
	}
	var leaked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE details::text LIKE '%'||$1||'%' OR details::text LIKE '%'||$2||'%') OR EXISTS(SELECT 1 FROM admin_changes WHERE summary::text LIKE '%'||$1||'%' OR summary::text LIKE '%REANA%')`, key.Key, created.Key).Scan(&leaked); err != nil || leaked {
		t.Fatal("history or persisted previews contain an issued secret or label", err)
	}
}

func TestUserKeyIssuanceSiteLimits(t *testing.T) {
	f := startIssuance(t, false, func(cfg *config.Config) {
		cfg.Auth.MaxActiveKeysPerUser = 2
		cfg.AdminAPI.UserKeyIssuance = config.UserKeyIssuance{AllowedGroups: []string{"students"}, MaxLifetimeDays: 7, LabelPrefix: "Hub · "}
	})
	_, secret := f.adminKey(t, "user_keys:issue")
	// Without the group-sync account an unknown subject is not found.
	f.issue(t, secret, map[string]any{"subject": "never-signed-in"}, 404, "user_not_found")

	f.browser(t).login(t, "staff-only", []string{"staff"})
	f.issue(t, secret, map[string]any{"subject": "staff-only"}, 403, "user_not_eligible")

	student := f.browser(t).login(t, "student", []string{"students"})
	f.issue(t, secret, map[string]any{"subject": "student", "expires_in_days": 8}, 400, "invalid_request_error")
	first := f.issue(t, secret, map[string]any{"subject": "student"}, 201, "")
	if lifetime := time.Until(first.Expires); first.Label != "Hub · REANA" || lifetime < 6*24*time.Hour || lifetime > 7*24*time.Hour {
		t.Fatalf("site prefix or lifetime cap ignored: %q %s", first.Label, lifetime)
	}
	f.issue(t, secret, map[string]any{"subject": "student", "expires_in_days": 7}, 201, "")
	// Issued keys count against auth.max_active_keys_per_user.
	f.issue(t, secret, map[string]any{"subject": "student"}, 409, "key_limit_error")
	if f.keys(t, student) != 2 {
		t.Fatal("active-key cap exceeded")
	}
	var refused int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='user_key.issue' AND result='refused'`).Scan(&refused); err != nil || refused != 1 {
		t.Fatal("refused issuance was not recorded", refused, err)
	}
}
