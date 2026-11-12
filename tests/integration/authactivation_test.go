package integration_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

// switchRecorder stands in for the router's API-token and group-sync
// services and records which issuer each switch selected.
type switchRecorder struct {
	mu      sync.Mutex
	issuers []string
}

func (s *switchRecorder) Switch(cfg *config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	issuer := ""
	if cfg.OIDC != nil {
		issuer = cfg.OIDC.Issuer
	}
	s.issuers = append(s.issuers, issuer)
}

func (s *switchRecorder) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.issuers) == 0 {
		return ""
	}
	return s.issuers[len(s.issuers)-1]
}

// ssoLogin signs in through a specific provider and returns the router user.
func ssoLogin(t *testing.T, b *portalBrowser, provider *identityProvider, subject string) string {
	t.Helper()
	resp, _ := b.call(t, "GET", "/portal/login", "", "")
	location, err := resp.Location()
	if resp.StatusCode != 303 || err != nil || !strings.HasPrefix(location.String(), provider.issuer) {
		t.Fatalf("login did not go to %s: %d %v", provider.issuer, resp.StatusCode, location)
	}
	q := location.Query()
	code := provider.authorize(t, q, subject, []string{"students"}, nil)
	resp, _ = b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != 303 {
		t.Fatal("callback", resp.StatusCode)
	}
	resp, data := b.call(t, "GET", "/portal/api/me", "", "")
	var me struct{ User struct{ ID string } }
	if resp.StatusCode != 200 || json.Unmarshal(data, &me) != nil {
		t.Fatal("me after SSO login", resp.StatusCode)
	}
	return me.User.ID
}

func TestLiveAuthenticationActivationRollbackAndIssuerSeparation(t *testing.T) {
	services := &switchRecorder{}
	environment := map[string]string{config.EnvOIDCClientSecret: "test-secret"}
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, _ *proxy.Handler) {
		cfg.OIDC.SyncClientID, cfg.OIDC.SyncIntervalS, cfg.OIDC.DefaultSignIn = "olr-sync", 900, "keycloak"
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays, RunAt: "03:00"}
		cfg.AuthSources = map[string]string{}
		for _, setting := range config.AuthSettings {
			cfg.AuthSources[setting.Name] = config.SourceDefault
		}
		cfg.AuthSources["portal_client_secret"] = config.SourceEnvironment
		h.Settings = &settings.AuthenticationService{DB: pool, File: cfg, Lookup: func(name string) (string, bool) { value, ok := environment[name]; return value, ok }}
		h.AuthServices = services
	}})
	providerA, providerB, providerC := f.provider, newIdentityProvider(t), newIdentityProvider(t)
	base := f.cfg.Portal.BaseURL
	path := "/portal/api/admin/settings/authentication"
	admin := signInLocalAdmin(t, f)
	call := func(method, suffix string, body any, want int) authenticationResponse {
		t.Helper()
		encoded := ""
		if body != nil {
			data, _ := json.Marshal(body)
			encoded = string(data)
		}
		origin := ""
		if method != "GET" {
			origin = base
		}
		resp, data := admin.call(t, method, path+suffix, encoded, origin)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, suffix, resp.StatusCode, want, data)
		}
		var view authenticationResponse
		_ = json.Unmarshal(data, &view)
		return view
	}
	type runtimeView struct {
		Runtime struct {
			Revision      int64
			SSOConfigured bool `json:"sso_configured"`
		}
		OtherIssuers []struct {
			Issuer       string
			EnabledUsers int64 `json:"enabled_users"`
		} `json:"other_issuers"`
	}
	runtime := func() runtimeView {
		t.Helper()
		_, data := admin.call(t, "GET", path, "", "")
		var view runtimeView
		_ = json.Unmarshal(data, &view)
		return view
	}
	// draft saves a document for an issuer and passes its checks and test sign-in.
	draft := func(issuer *identityProvider, mutate func(*settings.Authentication), check bool) (int64, int64) {
		t.Helper()
		view := call("GET", "", nil, 200)
		document := *view.Settings.Active.Document
		if issuer != nil {
			document.Issuer = issuer.issuer
		}
		if mutate != nil {
			mutate(&document)
		}
		view = call("PUT", "/draft", map[string]any{"version": view.Settings.Version, "document": document}, 200)
		id := view.Settings.Draft.ID
		if check {
			call("POST", "/checks", map[string]any{"revision": id}, 200)
			resp, data := admin.call(t, "POST", path+"/test-sign-in", `{"revision":`+jsonNumber(id)+`}`, base)
			var started struct{ URL string }
			if resp.StatusCode != 200 || json.Unmarshal(data, &started) != nil {
				t.Fatal("test sign-in", resp.StatusCode, string(data))
			}
			authorize, _ := url.Parse(started.URL)
			code := issuer.authorize(t, authorize.Query(), "tester", []string{"students"}, nil)
			admin.call(t, "GET", "/portal/callback?"+url.Values{"state": {authorize.Query().Get("state")}, "code": {code}}.Encode(), "", "")
		}
		return id, call("GET", "", nil, 200).Settings.Version
	}

	call("POST", "/import", struct{}{}, 200)
	imported := runtime()
	if imported.Runtime.Revision != 0 {
		t.Fatal("import must not change what the router runs")
	}
	alice := f.browser(t)
	aliceA := ssoLogin(t, alice, providerA, "alice")

	// An unchecked draft cannot be activated; nothing changes.
	revisionB, version := draft(providerB, nil, false)
	call("POST", "/activate", revisionBody(version, revisionB), 400)
	if runtime().Runtime.Revision != imported.Runtime.Revision || services.last() != "" {
		t.Fatal("an unchecked activation changed the runtime")
	}
	call("POST", "/discard", map[string]any{"version": version}, 200)

	// A checked draft for realm B activates live, without a restart.
	revisionB, version = draft(providerB, nil, true)
	call("POST", "/activate", revisionBody(version, revisionB), 200)
	if runtime().Runtime.Revision != revisionB || services.last() != providerB.issuer {
		t.Fatal("activation did not switch the runtime and services")
	}
	// Realm A's session no longer authenticates; realm B's same-named user is
	// a different account.
	if resp, _ := alice.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("an old-issuer session still authenticates", resp.StatusCode)
	}
	aliceB := ssoLogin(t, f.browser(t), providerB, "alice")
	if aliceB == aliceA {
		t.Fatal("accounts of different issuers were merged")
	}
	others := runtime().OtherIssuers
	if len(others) != 1 || others[0].Issuer != providerA.issuer || others[0].EnabledUsers < 1 {
		t.Fatal("previous issuer accounts are not reported", others)
	}

	// A provider that is down at activation time is refused; B stays active.
	revisionC, version := draft(providerC, nil, true)
	providerC.server.Close()
	call("POST", "/activate", revisionBody(version, revisionC), 400)
	if view := call("GET", "", nil, 200); view.Settings.Active.ID != revisionB || runtime().Runtime.Revision != revisionB {
		t.Fatal("a failed activation changed the active or running revision")
	}
	ssoLogin(t, f.browser(t), providerB, "bob")
	call("POST", "/discard", map[string]any{"version": version}, 200)

	// Roll back to the imported revision: realm A signs in again.
	version = call("GET", "", nil, 200).Settings.Version
	call("POST", "/rollback", revisionBody(version, revisionC), 404) // never active
	view := call("GET", "", nil, 200)
	importID := view.Settings.History[len(view.Settings.History)-1].ID
	call("POST", "/rollback", revisionBody(view.Settings.Version, importID), 200)
	if runtime().Runtime.Revision != importID || services.last() != providerA.issuer {
		t.Fatal("rollback did not restore the runtime")
	}
	if id := ssoLogin(t, f.browser(t), providerA, "alice"); id != aliceA {
		t.Fatal("rollback did not restore the realm A account")
	}

	// Turning SSO off needs no checks and keeps local administration.
	revisionOff, version := draft(nil, func(a *settings.Authentication) { a.Enabled = false }, false)
	call("POST", "/activate", revisionBody(version, revisionOff), 200)
	if runtime().Runtime.SSOConfigured || services.last() != "" {
		t.Fatal("SSO is still configured")
	}
	if resp, _ := f.browser(t).call(t, "GET", "/portal/login", "", ""); resp.StatusCode != 303 {
		t.Fatal("login without SSO must redirect to local sign-in", resp.StatusCode)
	}
	admin.localLogin(t, "admin", rotatedAdminPassword, 200)
	call("GET", "", nil, 200)
}

func revisionBody(version, revision int64) map[string]int64 {
	return map[string]int64{"version": version, "revision": revision}
}

func jsonNumber(n int64) string {
	data, _ := json.Marshal(n)
	return string(data)
}
