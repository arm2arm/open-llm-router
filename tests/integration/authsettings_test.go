package integration_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

// signInLocalAdmin bootstraps the protected administrator, completes the
// required password change and returns a browser with a fresh local session.
func signInLocalAdmin(t *testing.T, f *portalFixture) *portalBrowser {
	t.Helper()
	admin := auth.Admin{Pool: f.pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	b := f.browser(t)
	b.localLogin(t, "admin", rotatedAdminPassword, 200)
	return b
}

type authenticationResponse struct {
	Settings settings.View `json:"settings"`
	URLs     struct {
		Redirect   string `json:"redirect_uri"`
		PostLogout string `json:"post_logout_redirect_uri"`
	} `json:"urls"`
	CanChange bool `json:"can_change"`
}

func TestAdminAuthenticationSettingsAPI(t *testing.T) {
	const syncSecret = "sync-secret"
	environment := map[string]string{config.EnvOIDCClientSecret: "test-secret"}
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool) {
		// The fixture's portal secret comes "from the environment".
		cfg.OIDC.SyncClientID, cfg.OIDC.SyncIntervalS, cfg.OIDC.DefaultSignIn = "olr-sync", 900, "keycloak"
		// The fixture is built by hand; give it the defaults Parse would.
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays, RunAt: "03:00"}
		cfg.AuthSources = map[string]string{}
		for _, setting := range config.AuthSettings {
			cfg.AuthSources[setting.Name] = config.SourceDefault
		}
		cfg.AuthSources["portal_client_secret"] = config.SourceEnvironment
		cipher, err := settings.LoadCipher(func(string) string { return settingsKey(5) })
		if err != nil {
			t.Fatal(err)
		}
		h.Settings = &settings.Service{DB: pool, File: cfg, Cipher: cipher,
			Lookup: func(name string) (string, bool) { value, ok := environment[name]; return value, ok }}
	}})
	base := f.cfg.Portal.BaseURL
	path := "/portal/api/admin/settings/authentication"
	get := func(b *portalBrowser, want int) authenticationResponse {
		t.Helper()
		resp, data := b.call(t, "GET", path, "", "")
		if resp.StatusCode != want {
			t.Fatalf("GET settings: %d %s", resp.StatusCode, data)
		}
		var view authenticationResponse
		if want == 200 && json.Unmarshal(data, &view) != nil {
			t.Fatal("settings view", string(data))
		}
		if strings.Contains(string(data), syncSecret) {
			t.Fatal("settings view exposed a secret")
		}
		return view
	}
	post := func(b *portalBrowser, method, suffix string, body any, want int) []byte {
		t.Helper()
		encoded, _ := json.Marshal(body)
		resp, data := b.call(t, method, path+suffix, string(encoded), base)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, suffix, resp.StatusCode, want, data)
		}
		if strings.Contains(string(data), syncSecret) {
			t.Fatal("settings response exposed a secret")
		}
		return data
	}

	// Unauthenticated and non-admin callers are refused.
	if resp, _ := f.browser(t).call(t, "GET", path, "", ""); resp.StatusCode != 401 {
		t.Fatal("unauthenticated settings access", resp.StatusCode)
	}
	student := f.browser(t)
	student.login(t, "student-one", []string{"students"})
	get(student, 403)
	// An SSO administrator may look but not change how people sign in.
	ssoAdmin := f.browser(t)
	ssoAdmin.login(t, "sso-admin", []string{"admins"})
	if view := get(ssoAdmin, 200); view.CanChange {
		t.Fatal("an SSO session may not change authentication settings")
	}
	post(ssoAdmin, "POST", "/import", struct{}{}, 403)

	admin := signInLocalAdmin(t, f)
	view := get(admin, 200)
	if view.Settings.Managed || !view.CanChange || view.URLs.Redirect != base+"/portal/callback" || view.URLs.PostLogout != base+"/" {
		t.Fatal("initial view", view)
	}
	// A stale local session cannot change settings either.
	if _, err := f.pool.Exec(t.Context(), `UPDATE sessions SET created=created-interval '1 hour' WHERE auth_method='local'`); err != nil {
		t.Fatal(err)
	}
	post(admin, "POST", "/import", struct{}{}, 403)
	admin.localLogin(t, "admin", rotatedAdminPassword, 200)

	// Import, then save a draft that stores the sync secret.
	var imported authenticationResponse
	if json.Unmarshal(post(admin, "POST", "/import", struct{}{}, 200), &imported) != nil || !imported.Settings.Managed {
		t.Fatal("import")
	}
	document := *imported.Settings.Active.Document
	document.DefaultSignIn = "local"
	post(admin, "PUT", "/draft", map[string]any{"version": imported.Settings.Version, "document": document}, 200)
	view = get(admin, 200)
	// The import's version is now stale.
	post(admin, "PUT", "/draft", map[string]any{"version": imported.Settings.Version, "document": document}, 409)
	post(admin, "PUT", "/draft", map[string]any{"version": view.Settings.Version, "document": map[string]any{"issuer": "x", "unknown": true}}, 400)
	var saved authenticationResponse
	if json.Unmarshal(post(admin, "PUT", "/draft", map[string]any{"version": view.Settings.Version, "document": document,
		"secrets": map[string]string{"sync_client_secret": syncSecret}}, 200), &saved) != nil {
		t.Fatal("draft")
	}
	draft := saved.Settings.Draft
	if draft == nil || draft.Document.SyncClientSecret.Stored == "" || draft.Document.DefaultSignIn != "local" {
		t.Fatal("draft view", draft)
	}
	for _, secret := range draft.Secrets {
		if secret.Setting == "sync_client_secret" && (!secret.Configured || secret.Reference != "stored" || !secret.Available) {
			t.Fatal("stored secret state", secret)
		}
	}

	// Connection checks against the fake provider: discovery works; the sync
	// account cannot list users there. Results name no secret.
	var checked struct {
		Results []struct{ Name, Status, Message string }
	}
	if json.Unmarshal(post(admin, "POST", "/checks", map[string]any{"revision": draft.ID}, 200), &checked) != nil {
		t.Fatal("checks")
	}
	results := map[string]string{}
	for _, result := range checked.Results {
		results[result.Name] = result.Status
	}
	if results["discovery"] != "passed" || results["logout_metadata"] != "passed" || results["sync_permissions"] != "failed" {
		t.Fatal("check results", checked.Results)
	}

	// Test sign-in with the draft: verifies claims, creates no user or session.
	testSignIn := func(alter func(map[string]any)) (string, string) {
		t.Helper()
		var started struct{ URL string }
		if json.Unmarshal(post(admin, "POST", "/test-sign-in", map[string]any{"revision": draft.ID}, 200), &started) != nil {
			t.Fatal("start test sign-in")
		}
		authorize, err := url.Parse(started.URL)
		if err != nil || !strings.HasPrefix(started.URL, f.provider.issuer) {
			t.Fatal("test sign-in URL", started.URL)
		}
		q := authorize.Query()
		code := f.provider.authorize(t, q, "test-signin-user", []string{"staff", "not-configured-here"}, alter)
		resp, _ := admin.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
		location, _ := resp.Location()
		if resp.StatusCode != 303 || location == nil || location.Path != "/portal/admin" {
			t.Fatal("test callback", resp.StatusCode, location)
		}
		for _, c := range resp.Cookies() {
			if c.Name == "olr_session" {
				t.Fatal("a test sign-in replaced the administrator's session")
			}
		}
		result := get(admin, 200).Settings.Draft.Checks.TestSignIn
		return location.Query().Get("authentication_test"), result.Status + ": " + result.Message
	}
	status, message := testSignIn(nil)
	if status != "passed" || !strings.Contains(message, "2 groups (1 configured here)") || strings.Contains(message, "test-signin-user") {
		t.Fatal("test sign-in", status, message)
	}
	var created int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE username='test-signin-user'`).Scan(&created); err != nil || created != 0 {
		t.Fatal("a test sign-in created a user", err)
	}
	if status, message = testSignIn(func(claims map[string]any) { delete(claims, "groups") }); status != "failed" || !strings.Contains(message, "groups claim") {
		t.Fatal("missing groups claim", status, message)
	}
	// The administrator's local session still works afterwards.
	get(admin, 200)
	// Only a fresh local admin session can complete a test sign-in.
	var started struct{ URL string }
	_ = json.Unmarshal(post(admin, "POST", "/test-sign-in", map[string]any{"revision": draft.ID}, 200), &started)
	if _, err := f.pool.Exec(t.Context(), `UPDATE sessions SET created=created-interval '1 hour' WHERE auth_method='local'`); err != nil {
		t.Fatal(err)
	}
	authorize, _ := url.Parse(started.URL)
	code := f.provider.authorize(t, authorize.Query(), "late-user", []string{"staff"}, nil)
	resp, _ := admin.call(t, "GET", "/portal/callback?"+url.Values{"state": {authorize.Query().Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("stale session completed a test sign-in", resp.StatusCode)
	}

	// Default sign-in choice is reported to the sign-in page.
	_, methods := f.browser(t).call(t, "GET", "/portal/api/auth", "", "")
	if !strings.Contains(string(methods), `"default":"oidc"`) {
		t.Fatal("default sign-in", string(methods))
	}
	var actions string
	if err := f.pool.QueryRow(t.Context(), `SELECT string_agg(action||':'||result, ' ' ORDER BY id) FROM audit_events`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"settings.import:succeeded", "settings.draft_save:succeeded", "settings.check:succeeded", "settings.test_sign_in:succeeded"} {
		if !strings.Contains(actions, want) {
			t.Fatal("audit", want, actions)
		}
	}
}
