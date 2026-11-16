package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLocalAdministratorAuthorityRequiresFreshLocalSession(t *testing.T) {
	f := startPortal(t)
	local := signInLocalAdmin(t, f)
	sso := f.browser(t)
	sso.login(t, "sso-operator", []string{"admins"})
	deputy := createPortalUser(t, local, "deputy", []string{"admins"})
	reader := createPortalUser(t, local, "reader", []string{"staff"})
	create := func(name string, groups []string) any {
		return map[string]any{"username": name, "groups": groups, "password": userTemporaryPassword}
	}
	adminCall(t, sso, "POST", "/portal/api/admin/users", create("sso-deputy", []string{"admins"}), 403)
	adminCall(t, sso, "PUT", "/portal/api/admin/users/"+reader+"/groups", map[string]any{"groups": []string{"admins"}}, 403)
	adminCall(t, sso, "POST", "/portal/api/admin/users/"+deputy+"/password", map[string]any{"password": userResetPassword}, 403)
	createPortalUser(t, sso, "ordinary", []string{"staff"})
	adminCall(t, sso, "POST", "/portal/api/admin/users/"+reader+"/password", map[string]any{"password": userResetPassword}, 204)
	adminCall(t, local, "PUT", "/portal/api/admin/users/"+reader+"/groups", map[string]any{"groups": []string{"admins"}}, 204)
	adminCall(t, local, "POST", "/portal/api/admin/users/"+deputy+"/password", map[string]any{"password": userResetPassword}, 204)
	createPortalUser(t, local, "local-deputy", []string{"admins"})
	if _, err := f.pool.Exec(t.Context(), `UPDATE sessions SET created=created-interval '1 hour' WHERE auth_method='local'`); err != nil {
		t.Fatal(err)
	}
	adminCall(t, local, "POST", "/portal/api/admin/users", create("stale-deputy", []string{"admins"}), 403)
	adminCall(t, local, "PUT", "/portal/api/admin/users/"+reader+"/groups", map[string]any{"groups": []string{"admins"}}, 403)
	adminCall(t, local, "POST", "/portal/api/admin/users/"+deputy+"/password", map[string]any{"password": userChangedPassword}, 403)
}

func TestSSOCannotGrantLocalAuthorityThroughRouting(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, forwarder *proxy.Handler) {
		cfg.OIDC.SyncIntervalS = 900
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays, RunAt: "03:00"}
		policy, err := backendpolicy.New(cfg, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		h.Routing = &settings.RoutingService{DB: pool, File: cfg, Tokens: policy.Token, Verify: endpoints.VerifyDestinations(policy), Current: func() *config.Config { current, _ := forwarder.Snapshot(); return current }}
		h.RoutingRuntime = forwarder
		h.Endpoints = &endpoints.Service{DB: pool, Base: cfg, Policy: policy, Runtime: forwarder}
	}})
	local := signInLocalAdmin(t, f)
	sso := f.browser(t)
	sso.login(t, "sso-operator", []string{"admins"})
	// A known local password must not become administrative by changing the
	// group policy instead of the user's memberships.
	createPortalUser(t, sso, "ordinary", []string{"staff"})
	path := "/portal/api/admin/settings/routing"
	var view routingResponse
	_ = json.Unmarshal(adminCall(t, local, "POST", path+"/import", struct{}{}, 200), &view)
	doc := view.Settings.Active.Document
	group := doc.Groups["staff"]
	group.Admin = true
	doc.Groups["staff"] = group
	_ = json.Unmarshal(adminCall(t, sso, "PUT", path+"/draft", map[string]any{"version": view.Settings.Version, "document": doc}, 200), &view)
	adminCall(t, local, "POST", path+"/checks", map[string]any{"revision": view.Settings.Draft.ID}, 200)
	_ = json.Unmarshal(adminCall(t, local, "GET", path, nil, 200), &view)
	body := map[string]any{"version": view.Settings.Version, "revision": view.Settings.Draft.ID}
	adminCall(t, sso, "POST", path+"/activate", body, 403)
	adminCall(t, local, "POST", path+"/activate", body, 200)
}

func TestCanonicalUserIDsProtectSelfDisableAndAudit(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	id := b.login(t, "operator", []string{"admins"})
	for _, spelling := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", "")} {
		adminCall(t, b, "PUT", "/portal/api/admin/users/"+spelling+"/enabled", map[string]bool{"enabled": false}, 400)
	}
	target := createPortalUser(t, b, "ordinary", []string{"staff"})
	adminCall(t, b, "PUT", "/portal/api/admin/users/"+strings.ToUpper(target)+"/enabled", map[string]bool{"enabled": false}, 204)
	var recorded string
	if err := f.pool.QueryRow(t.Context(), `SELECT target_id FROM audit_events WHERE action='user.enablement' ORDER BY id DESC LIMIT 1`).Scan(&recorded); err != nil || recorded != target {
		t.Fatalf("non-canonical audit target %q: %v", recorded, err)
	}
}

func TestAgentAdministratorTargetsRequireDelegation(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
	}})
	local := signInLocalAdmin(t, f)
	issue := func(permissions []string) string {
		data := adminCall(t, local, "POST", "/portal/api/admin/agent-keys", map[string]any{"label": "authority regression", "permissions": permissions, "allowed_networks": []string{"127.0.0.1"}, "expires_in_days": 1}, 201)
		var out struct{ Key string }
		if json.Unmarshal(data, &out) != nil || out.Key == "" {
			t.Fatal("missing diagnostic key")
		}
		return out.Key
	}
	helpdesk := issue([]string{"users:read", "users:credentials"})
	operator := issue([]string{"users:read", "users:write"})
	delegated := issue([]string{"users:read", "users:write", "users:credentials", "admin_keys:delegate"})
	call := func(key, method, path string, body any, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, err := http.NewRequestWithContext(t.Context(), method, f.router.URL+"/api/admin/v1"+path, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s status %d want %d", path, response.StatusCode, want)
		}
		return data
	}
	args := func(id string, body any) any {
		return map[string]any{"parameters": map[string]string{"id": id}, "body": body}
	}
	preview := func(key, op string, arguments any, want int) string {
		t.Helper()
		data := call(key, "POST", "/changes/preview", map[string]any{"operation": op, "arguments": arguments}, want)
		var out struct{ ID string }
		_ = json.Unmarshal(data, &out)
		return out.ID
	}
	apply := func(key, id string, arguments any, want int) {
		t.Helper()
		call(key, "POST", "/changes/"+id+"/apply", map[string]any{"arguments": arguments, "idempotency_key": "authority-test-" + id}, want)
	}
	deputy := createPortalUser(t, local, "deputy", []string{"admins"})
	apiOnly, err := (auth.Admin{Pool: f.pool}).AddUser(t.Context(), "api-only-admin", "", []string{"admins"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{deputy, apiOnly} {
		arguments := args(id, map[string]string{"password": userResetPassword})
		preview(helpdesk, "reset_user_password", arguments, 403)
		change := preview(delegated, "reset_user_password", arguments, 201)
		apply(delegated, change, arguments, 204)
		if err := (auth.Admin{Pool: f.pool}).SetEnabled(t.Context(), id, false); err != nil {
			t.Fatal(err)
		}
		arguments = args(id, map[string]bool{"enabled": true})
		preview(operator, "set_user_enabled", arguments, 403)
		change = preview(delegated, "set_user_enabled", arguments, 201)
		apply(delegated, change, arguments, 204)
		arguments = args(id, map[string]bool{"enabled": false})
		change = preview(operator, "set_user_enabled", arguments, 201)
		apply(operator, change, arguments, 204)
	}
	ordinary := createPortalUser(t, local, "ordinary", []string{"staff"})
	arguments := args(ordinary, map[string]string{"password": userResetPassword})
	change := preview(helpdesk, "reset_user_password", arguments, 201)
	apply(helpdesk, change, arguments, 204)
	for _, enabled := range []bool{false, true} {
		enablement := args(ordinary, map[string]bool{"enabled": enabled})
		id := preview(operator, "set_user_enabled", enablement, 201)
		apply(operator, id, enablement, 204)
	}
	// A target promoted after preview must be rechecked at apply, not trusted
	// merely because an earlier preview was permitted.
	arguments = args(ordinary, map[string]string{"password": userChangedPassword})
	change = preview(helpdesk, "reset_user_password", arguments, 201)
	if err := (auth.Admin{Pool: f.pool}).SetLocalGroups(t.Context(), ordinary, []string{"admins"}); err != nil {
		t.Fatal(err)
	}
	apply(helpdesk, change, arguments, 403)
	operations := call(helpdesk, "GET", "/operations", nil, 200)
	if !strings.Contains(string(operations), `"administrator_target_permission":"admin_keys:delegate"`) {
		t.Fatal("conditional permission missing from operation discovery")
	}
}
