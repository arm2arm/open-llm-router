package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAgentScopesBindingAttributionAndStableSSOPreviews(t *testing.T) {
	f := startPortal(t, usageOptions{
		portalConfig: func(cfg *config.Config) { cfg.AdminAPI.Enabled = true },
		portal: func(h *portal.Handler, cfg *config.Config, db *pgxpool.Pool, p *proxy.Handler) {
			cfg.Forwarders.UnknownUser = config.UnknownUserReject
			cfg.OIDC.SyncIntervalS = 900
			cfg.Retention = config.Retention{UsageDays: 90, UsageDailyDays: 400, RunAt: "03:00"}
			h.Routing = &settings.RoutingService{DB: db, File: cfg, Current: func() *config.Config { current, _ := p.Snapshot(); return current }}
			h.RoutingRuntime = p
		},
	})
	admin := signInLocalAdmin(t, f)
	sso := f.browser(t)
	ssoID := sso.login(t, "sync-preview-subject", []string{"staff"})
	accounts := auth.Admin{Pool: f.pool}
	localID, err := accounts.CreateLocalUser(t.Context(), "quota-preview-local", "", []string{"staff"}, []byte("synthetic-preview-password"))
	if err != nil {
		t.Fatal(err)
	}
	initial := int64(321)
	if err := accounts.SetOverride(t.Context(), localID, "tokens_per_day", &initial, "initial preview quota", "operator"); err != nil {
		t.Fatal(err)
	}
	// A real draft affects an SSO user, so a routing read would disclose an
	// identity if permission filtering were missing.
	response, data := admin.call(t, "POST", "/portal/api/admin/settings/routing/import", `{}`, f.cfg.Portal.BaseURL)
	var routing struct{ Settings settings.RoutingView }
	if response.StatusCode != 200 || json.Unmarshal(data, &routing) != nil {
		t.Fatalf("routing import failed (%d): %s", response.StatusCode, data)
	}
	document, _ := json.Marshal(routing.Settings.Active.Document)
	var candidate map[string]any
	_ = json.Unmarshal(document, &candidate)
	candidate["groups"].(map[string]any)["staff"].(map[string]any)["tokens_per_day"] = 123
	save, _ := json.Marshal(map[string]any{"version": routing.Settings.Version, "document": candidate})
	response, _ = admin.call(t, "PUT", "/portal/api/admin/settings/routing/draft", string(save), f.cfg.Portal.BaseURL)
	if response.StatusCode != 200 {
		t.Fatal("draft save failed", response.StatusCode)
	}
	type issued struct {
		Key      string
		Metadata adminaccess.Key
	}
	issue := func(permissions []string) issued {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"label": "follow-up agent", "permissions": permissions, "allowed_networks": []string{"127.0.0.1"}, "expires_in_days": 1})
		response, data := admin.call(t, "POST", "/portal/api/admin/agent-keys", string(body), f.cfg.Portal.BaseURL)
		var key issued
		if response.StatusCode != 201 || json.Unmarshal(data, &key) != nil {
			t.Fatal("agent issuance failed")
		}
		return key
	}
	writer, reader := issue(adminaccess.Permissions), issue([]string{"routing:read"})
	call := func(key, method, path string, body any, want int) []byte {
		t.Helper()
		encoded, _ := json.Marshal(body)
		request, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s returned %d, want %d (credential-bearing body omitted)", path, response.StatusCode, want)
		}
		return data
	}
	full := call(writer.Key, "POST", "/api/admin/v1/read/get_routing", map[string]any{}, 200)
	if !strings.Contains(string(full), "sync-preview-subject") {
		t.Fatal("test draft did not affect a known user")
	}
	limited := call(reader.Key, "POST", "/api/admin/v1/read/get_routing", map[string]any{}, 200)
	for _, identity := range []string{ssoID, localID, "sync-preview-subject", "quota-preview-local"} {
		if strings.Contains(string(limited), identity) {
			t.Fatal("routing-only key disclosed a stored user")
		}
	}
	var restricted struct {
		Settings        settings.RoutingView
		AffectedUsers   []any
		ImpactAvailable bool
	}
	if json.Unmarshal(limited, &restricted) != nil || len(restricted.AffectedUsers) != 0 || restricted.ImpactAvailable || restricted.Settings.Active.CreatedBy != "" || restricted.Settings.Draft.CreatedBy != "" {
		t.Fatal("routing-only view retained identity metadata")
	}
	for _, revision := range restricted.Settings.History {
		if revision.CreatedBy != "" {
			t.Fatal("routing-only history retained an actor identity")
		}
	}
	call(reader.Key, "POST", "/api/admin/v1/read/preview_routing_policies", map[string]any{"body": map[string]any{"user_id": ssoID}}, 403)
	hypothetical := call(reader.Key, "POST", "/api/admin/v1/read/preview_routing_policies", map[string]any{"body": map[string]any{"memberships": []string{"staff"}}}, 200)
	if strings.Contains(string(hypothetical), ssoID) || !strings.Contains(string(hypothetical), `"affected_users":[]`) {
		t.Fatal("hypothetical preview leaked stored users")
	}
	// Read tools accept all three valid empty-argument forms. The routing-only
	// permission still applies when the same operation is invoked through MCP.
	for _, arguments := range []string{"omitted", "null", "object"} {
		params := map[string]any{"name": "get_routing"}
		if arguments == "null" {
			params["arguments"] = nil
		} else if arguments == "object" {
			params["arguments"] = map[string]any{}
		}
		body := call(reader.Key, "POST", "/api/admin/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": arguments, "method": "tools/call", "params": params}, 200)
		var rpc struct {
			Result struct {
				IsError bool
				Content []struct{ Text string }
			}
			Error any
		}
		if json.Unmarshal(body, &rpc) != nil || rpc.Error != nil || rpc.Result.IsError || len(rpc.Result.Content) != 1 || strings.Contains(rpc.Result.Content[0].Text, ssoID) {
			t.Fatal("empty-argument MCP read failed or bypassed its scope", arguments)
		}
	}
	preview := func(operation string, args any) string {
		t.Helper()
		body := call(writer.Key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": operation, "arguments": args}, 201)
		var change struct{ ID string }
		if json.Unmarshal(body, &change) != nil || change.ID == "" {
			t.Fatal("missing preview identifier")
		}
		return change.ID
	}
	apply := func(id string, args any, want int) {
		call(writer.Key, "POST", "/api/admin/v1/changes/"+id+"/apply", map[string]any{"arguments": args, "idempotency_key": id}, want)
	}
	override := map[string]any{"parameters": map[string]string{"id": localID, "field": "tokens_per_day"}, "body": map[string]any{"value": 444, "reason": "verified agent attribution"}}
	body := call(writer.Key, "POST", "/api/admin/v1/changes/preview", map[string]any{"operation": "set_user_override", "arguments": override}, 201)
	var change struct {
		ID       string
		Proposed struct {
			Before struct {
				User struct {
					HasLocalPassword   bool `json:"has_local_password"`
					MustChangePassword bool `json:"must_change_password"`
				}
				Limits struct {
					TokensPerDay int64 `json:"tokens_per_day"`
				}
			}
		}
	}
	if json.Unmarshal(body, &change) != nil || change.Proposed.Before.Limits.TokensPerDay != 321 || !change.Proposed.Before.User.HasLocalPassword || !change.Proposed.Before.User.MustChangePassword {
		t.Fatal("before-image lost real token quotas or password flags")
	}
	var before, after int64
	if err := f.pool.QueryRow(t.Context(), `SELECT version FROM admin_state WHERE id=true`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	sso.login(t, "sync-preview-subject", []string{"staff"})
	if err := f.pool.QueryRow(t.Context(), `SELECT version FROM admin_state WHERE id=true`).Scan(&after); err != nil || after != before {
		t.Fatal("unchanged SSO claims invalidated a pending preview", err)
	}
	apply(change.ID, override, 204)
	var attribution string
	if err := f.pool.QueryRow(t.Context(), `SELECT set_by FROM quota_overrides WHERE user_id=$1 AND field='tokens_per_day'`, localID).Scan(&attribution); err != nil || attribution != "agent:"+writer.Metadata.ID {
		t.Fatal("override did not identify its administration key", err)
	}
	stale := preview("set_user_override", override)
	sso.login(t, "sync-preview-subject", []string{"students"})
	apply(stale, override, 409)
	stale = preview("set_user_override", override)
	if err := accounts.SetEnabled(t.Context(), ssoID, false); err != nil {
		t.Fatal(err)
	}
	apply(stale, override, 409)
	passwordArgs := map[string]any{"parameters": map[string]string{"id": localID}, "body": map[string]any{"password": "synthetic-new-temporary-password"}}
	passwordPreview := preview("reset_user_password", passwordArgs)
	var binding []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT arguments_hash FROM admin_changes WHERE id=$1`, passwordPreview).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(passwordArgs)
	plain := sha256.Sum256(encoded)
	if bytes.Equal(binding, plain[:]) {
		t.Fatal("password preview retained an offline SHA-256 oracle")
	}
	apply(passwordPreview, passwordArgs, 204)
	invalid := call(writer.Key, "POST", "/api/admin/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "preview_change", "arguments": map[string]any{"operation": "set_user_override", "arguments": map[string]any{"parameters": map[string]string{"id": localID, "field": "tokens_per_day"}, "body": map[string]any{"value": "not-a-number", "reason": "invalid"}}}}}, 200)
	if !strings.Contains(string(invalid), `"isError":true`) {
		t.Fatal("MCP accepted invalid operation-specific arguments")
	}
	response, _ = admin.call(t, "PUT", "/portal/api/admin/users/"+localID+"/overrides/tokens_per_day", `{"value":777,"reason":"portal attribution"}`, f.cfg.Portal.BaseURL)
	if response.StatusCode != 204 {
		t.Fatal("portal override failed")
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT set_by FROM quota_overrides WHERE user_id=$1 AND field='tokens_per_day'`, localID).Scan(&attribution); err != nil || attribution != "portal:admin" {
		t.Fatal("portal attribution changed", err)
	}
}
