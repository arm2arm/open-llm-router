package integration_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

func startLocalPortal(t *testing.T, oidc *config.OIDC) *portalFixture {
	t.Helper()
	f := &portalFixture{}
	f.usageRouter = startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), usageOptions{
		configure: func(cfg *config.Config, forwarder *proxy.Handler, s *server.Server, pool *pgxpool.Pool) {
			cfg.OIDC = oidc
			cfg.OIDCClientSecret = "test-secret"
			cfg.Portal = config.Portal{BaseURL: "http://portal.example.test", CookieSecure: false}
			cfg.Groups["admins"] = config.Group{Admin: true, Models: []string{"*"}}
			h, err := portal.New(t.Context(), cfg, pool, forwarder.Limits)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.Close)
			h.Pools = forwarder.Pools
			s.Handle("/portal", h)
			s.Handle("/portal/", h)
			f.cfg = cfg
		},
	})
	return f
}

func (b *portalBrowser) localLogin(t *testing.T, username, password string, want int) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, data := b.call(t, "POST", "/portal/login/local", string(body), b.f.cfg.Portal.BaseURL)
	if resp.StatusCode != want {
		t.Fatalf("local login status=%d want=%d body=%s", resp.StatusCode, want, data)
	}
	if strings.Contains(string(data), password) || strings.Contains(string(data), "$argon2") {
		t.Fatal("login exposed credential")
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "olr_session" && cookie.MaxAge > 0 {
			if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/portal" {
				t.Fatal("unsafe local cookie")
			}
			return cookie
		}
	}
	if want == 200 {
		t.Fatal("local login did not set a session")
	}
	return nil
}

func setSessionCookie(b *portalBrowser, cookie *http.Cookie) {
	u, _ := url.Parse(b.f.cfg.Portal.BaseURL + "/portal/")
	b.client.Jar.SetCookies(u, []*http.Cookie{cookie})
}

func TestLocalPortalFirstChangeSessionRotationAndLogout(t *testing.T) {
	f := startLocalPortal(t, nil)
	admin := auth.Admin{Pool: f.pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	b := f.browser(t)
	resp, _ := b.call(t, "GET", "/portal/login/local", "", "")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/portal/sign-in/local" {
		t.Fatal("local route redirects to SSO")
	}
	restricted := b.localLogin(t, "admin", initialAdminPassword, 200)
	if restricted.MaxAge != 600 {
		t.Fatal("initial session is not short-lived")
	}
	var body []byte
	for _, path := range []string{"/portal/api/me", "/portal/api/admin/users", "/portal/api/keys", "/portal/api/models"} {
		resp, body = b.call(t, "GET", path, "", "")
		if resp.StatusCode != 403 || !strings.Contains(string(body), "password_change_required") {
			t.Fatal("restricted session accessed portal", path, resp.StatusCode)
		}
	}
	resp, body = b.call(t, "GET", "/portal/api/auth/session", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"must_change_password":true`) {
		t.Fatal("required-change status unavailable")
	}
	passwordChange := func(current, next, origin string, want int) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"current_password": current, "new_password": next})
		resp, data := b.call(t, "POST", "/portal/password", string(body), origin)
		if resp.StatusCode != want {
			t.Fatal("password change", resp.StatusCode, string(data))
		}
		if strings.Contains(string(data), current) || strings.Contains(string(data), next) {
			t.Fatal("password change exposed credentials")
		}
	}
	passwordChange(initialAdminPassword, rotatedAdminPassword, "https://evil.example", 403)
	passwordChange("incorrect private password", rotatedAdminPassword, f.cfg.Portal.BaseURL, 401)
	passwordChange(initialAdminPassword, "too short", f.cfg.Portal.BaseURL, 400)
	passwordChange(initialAdminPassword, initialAdminPassword, f.cfg.Portal.BaseURL, 400)
	passwordChange(initialAdminPassword, rotatedAdminPassword, f.cfg.Portal.BaseURL, 200)
	setSessionCookie(b, restricted)
	if resp, _ := b.call(t, "GET", "/portal/api/auth/session", "", ""); resp.StatusCode != 401 {
		t.Fatal("old restricted session survived rotation")
	}
	b.localLogin(t, "admin", initialAdminPassword, 401)
	full := b.localLogin(t, "admin", rotatedAdminPassword, 200)
	if full.MaxAge != 28800 || full.Value == restricted.Value {
		t.Fatal("full session was not rotated")
	}
	resp, body = b.call(t, "GET", "/portal/api/me", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"admin":true`) || !strings.Contains(string(body), `"auth_method":"local"`) {
		t.Fatal("local administrator lacks authority", resp.StatusCode)
	}
	resp, _ = b.call(t, "GET", "/portal/api/admin/users", "", "")
	if resp.StatusCode != 200 {
		t.Fatal("local Admin unavailable")
	}
	// Reauthentication rotates an existing identifier and deletes the old row.
	newSession := b.localLogin(t, "admin", rotatedAdminPassword, 200)
	if newSession.Value == full.Value {
		t.Fatal("session fixation")
	}
	stolen := f.browser(t)
	setSessionCookie(stolen, full)
	if resp, _ := stolen.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("replaced session remains valid")
	}
	resp, _ = b.call(t, "POST", "/portal/logout", `{}`, "https://evil.example")
	if resp.StatusCode != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	resp, body = b.call(t, "POST", "/portal/logout", `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"logout_url":"/portal/login/local"`) {
		t.Fatal("local logout depends on SSO")
	}
	setSessionCookie(b, newSession)
	if resp, _ := b.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("logged-out session survived")
	}
	// A session inserted after a concurrent password rotation still cannot use
	// the credential revision from before the rotation.
	stale := sha256.Sum256([]byte(newSession.Value))
	execSQL(t, f.pool, `INSERT INTO sessions(id_hash,user_id,expires,auth_method,credential_revision)
		VALUES($1,$2,now()+interval '1 hour','local',$3)`, stale[:], account.ID, account.CredentialRevision)
	setSessionCookie(b, newSession)
	if resp, _ := b.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("stale password revision authenticated")
	}
	if strings.Contains(f.logs.String(), initialAdminPassword) || strings.Contains(f.logs.String(), rotatedAdminPassword) {
		t.Fatal("credentials in request log")
	}
}

func TestLocalPortalCredentialErrorsThrottleExpiryAndIsolation(t *testing.T) {
	f := startLocalPortal(t, nil)
	admin := auth.Admin{Pool: f.pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	// A same-name SSO identity without a local credential is never adopted.
	if _, err := admin.SyncIdentity(t.Context(), "https://idp.example/realms/test", "sso-only", "sso-only", "same@example.org", nil); err != nil {
		t.Fatal(err)
	}
	b := f.browser(t)
	var generic string
	for _, username := range []string{"admin", "unknown", "sso-only"} {
		body, _ := json.Marshal(map[string]string{"username": username, "password": "incorrect private password"})
		resp, data := b.call(t, "POST", "/portal/login/local", string(body), f.cfg.Portal.BaseURL)
		if resp.StatusCode != 401 {
			t.Fatal("invalid credential status", resp.StatusCode)
		}
		if generic != "" && generic != string(data) {
			t.Fatal("credential errors reveal local identity existence")
		}
		generic = string(data)
	}
	for range 9 {
		b.localLogin(t, "admin", "incorrect private password", 401)
	}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": rotatedAdminPassword})
	resp, data := b.call(t, "POST", "/portal/login/local", string(body), f.cfg.Portal.BaseURL)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || strings.Contains(string(data), "admin") {
		t.Fatal("login guessing is not throttled", resp.StatusCode)
	}
	var enabled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT enabled FROM users WHERE id=$1`, account.ID).Scan(&enabled); err != nil || !enabled {
		t.Fatal("throttling disabled the protected account", err)
	}
	// Expire a fixed window without waiting a minute; a new attempt succeeds.
	execSQL(t, f.pool, `UPDATE portal_auth_throttle SET expires=now()-interval '1 second'`)
	session := b.localLogin(t, "admin", rotatedAdminPassword, 200)
	hash := sha256.Sum256([]byte(session.Value))
	execSQL(t, f.pool, `UPDATE sessions SET expires=now()-interval '1 second' WHERE id_hash=$1`, hash[:])
	if resp, _ := b.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("expired local session accepted")
	}
	b.localLogin(t, "admin", rotatedAdminPassword, 200)
	// Session cookies still cannot authenticate inference requests.
	if resp, _ := b.call(t, "GET", "/v1/models", "", ""); resp.StatusCode != 401 {
		t.Fatal("session authorized the inference API")
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+f.key)
	apiResp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	apiResp.Body.Close()
	if apiResp.StatusCode != 200 {
		t.Fatal("API keys regressed")
	}
	// Buckets must not store cleartext usernames, IP addresses or passwords.
	var bad int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM portal_auth_throttle WHERE octet_length(bucket)<>32`).Scan(&bad); err != nil || bad != 0 {
		t.Fatal("unsafe throttle storage", err)
	}
}

func TestLocalPortalDuringOIDCOutageAndRecovery(t *testing.T) {
	p := newIdentityProvider(t)
	target, _ := url.Parse(p.server.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var online atomic.Bool
	backchannel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !online.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer backchannel.Close()
	f := startLocalPortal(t, &config.OIDC{Issuer: p.issuer, PortalClientID: "olr-portal", GroupsClaim: "groups", UsernameClaim: "preferred_username", BackchannelURL: backchannel.URL + "/realms/test"})
	f.provider = p
	admin := auth.Admin{Pool: f.pool}
	account, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), account.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	b := f.browser(t)
	resp, body := b.call(t, "GET", "/portal/api/auth", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"configured":true`) || !strings.Contains(string(body), `"available":false`) {
		t.Fatal("OIDC outage not reported separately")
	}
	for _, path := range []string{"/portal/login", "/portal/callback?state=bogus&code=bogus"} {
		resp, _ := b.call(t, "GET", path, "", "")
		if resp.StatusCode != 503 || resp.Header.Get("Location") != "" {
			t.Fatal("unavailable SSO operation accepted")
		}
	}
	b.localLogin(t, "admin", rotatedAdminPassword, 200)
	if resp, _ := b.call(t, "GET", "/portal/api/admin/users", "", ""); resp.StatusCode != 200 {
		t.Fatal("outage blocked local Admin")
	}
	online.Store(true)
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, body = b.call(t, "GET", "/portal/api/auth", "", "")
		var methods struct{ OIDC struct{ Available bool } }
		if json.Unmarshal(body, &methods) == nil && methods.OIDC.Available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SSO did not recover without a restart")
		}
		time.Sleep(100 * time.Millisecond)
	}
	sso := f.browser(t)
	ssoID := sso.login(t, "admin", []string{"admins"})
	if ssoID == account.ID {
		t.Fatal("same-name SSO login became the protected local administrator")
	}
	resp, body = b.call(t, "POST", "/portal/logout", `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 200 || strings.Contains(string(body), p.issuer) {
		t.Fatal("local logout called Keycloak")
	}
	resp, body = sso.call(t, "POST", "/portal/logout", `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 200 || !strings.Contains(string(body), fmt.Sprintf("%s/logout", p.issuer)) {
		t.Fatal("working SSO logout regressed")
	}
}
