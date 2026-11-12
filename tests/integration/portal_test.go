package integration_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/activity"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5/pgxpool"
)

type portalFixture struct {
	*usageRouter
	provider *identityProvider
	cfg      *config.Config
	traces   *requesttrace.Service
	// The router and the portal use separate brokers, as two instances
	// would: portal events arrive through Postgres NOTIFY only.
	routerActivity, portalActivity *activity.Broker
}
type portalBrowser struct {
	f      *portalFixture
	client *http.Client
}

func startPortal(t *testing.T, options ...usageOptions) *portalFixture {
	t.Helper()
	p := newIdentityProvider(t)
	f := &portalFixture{provider: p}
	opt := usageOptions{}
	if len(options) > 0 {
		opt = options[0]
	}
	opt.configure = func(cfg *config.Config, forwarder *proxy.Handler, s *server.Server, pool *pgxpool.Pool) {
		cfg.OIDC = &config.OIDC{Issuer: p.issuer, PortalClientID: "olr-portal", GroupsClaim: "groups", UsernameClaim: "preferred_username"}
		cfg.OIDCClientSecret = "test-secret"
		cfg.Portal = config.Portal{BaseURL: "http://portal.example.test", CookieSecure: false}
		cfg.Groups["students"] = config.Group{Models: []string{"chat-model"}, RPM: limitPtr(20)}
		cfg.Groups["admins"] = config.Group{Models: []string{"*"}, Admin: true}
		h, err := portal.New(t.Context(), cfg, pool, forwarder.Limits)
		if err != nil {
			t.Fatal(err)
		}
		traces, err := requesttrace.New(t.Context(), pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(traces.Close)
		f.traces = traces
		forwarder.Traces = traces
		h.Traces = traces
		done := make(chan struct{})
		f.routerActivity = activity.New(done, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
		f.portalActivity = activity.New(done, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
		t.Cleanup(func() { close(done); f.routerActivity.Wait(); f.portalActivity.Wait() })
		forwarder.Activity = f.routerActivity
		traces.Activity = f.routerActivity
		h.Activity = f.portalActivity
		h.Pools = forwarder.Pools
		t.Cleanup(h.Close)
		s.Handle("/portal", h)
		s.Handle("/portal/", h)
		f.cfg = cfg
	}
	f.usageRouter = startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 7}), opt)
	// OIDC discovery is deliberately independent of portal startup.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := f.browser(t).call(t, "GET", "/portal/api/auth", "", "")
		var methods struct{ OIDC struct{ Available bool } }
		if json.Unmarshal(body, &methods) == nil && methods.OIDC.Available {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test OIDC provider did not become available")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f
}
func (f *portalFixture) browser(t *testing.T) *portalBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &portalBrowser{f: f, client: &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (b *portalBrowser) call(t *testing.T, method, path, body, origin string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, b.f.router.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Reach the test listener while presenting the configured public host,
	// as a reverse proxy preserving Host would do in a real deployment.
	base, err := url.Parse(b.f.cfg.Portal.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = base.Host
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-provider-token-marker") || strings.Contains(string(data), "id_token") || strings.Contains(string(data), "test-secret") {
		t.Fatal("portal exposed an OIDC token or secret")
	}
	if strings.HasPrefix(path, "/v1/") {
		if len(resp.Cookies()) != 0 {
			t.Fatal("inference route set a session cookie")
		}
	} else if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("portal response can be cached")
	}
	return resp, data
}
func (b *portalBrowser) challenge(t *testing.T) (url.Values, *http.Cookie) {
	t.Helper()
	resp, _ := b.call(t, "GET", "/portal/login", "", "")
	if resp.StatusCode != 303 {
		t.Fatal("login status", resp.StatusCode)
	}
	u, err := resp.Location()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "olr_login" {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/portal" || c.MaxAge != 600 {
				t.Fatal("unsafe login cookie")
			}
			return u.Query(), c
		}
	}
	t.Fatal("missing login cookie")
	return nil, nil
}
func (b *portalBrowser) login(t *testing.T, subject string, groups []string) string {
	t.Helper()
	q, _ := b.challenge(t)
	code := b.f.provider.authorize(t, q, subject, groups, nil)
	resp, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != 303 {
		t.Fatal("callback failed", resp.StatusCode)
	}
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "olr_session" {
			session = c
		}
	}
	if session == nil || !session.HttpOnly || session.Path != "/portal" || session.SameSite != http.SameSiteLaxMode || session.MaxAge != 28800 {
		t.Fatal("unsafe session cookie")
	}
	hash := sha256.Sum256([]byte(session.Value))
	var persisted []byte
	if err := b.f.pool.QueryRow(t.Context(), `SELECT id_hash FROM sessions WHERE id_hash=$1`, hash[:]).Scan(&persisted); err != nil || len(persisted) != 32 {
		t.Fatal("session ID was not hashed", err)
	}
	resp, data := b.call(t, "GET", "/portal/api/me", "", "")
	var me struct {
		User struct {
			ID, Username, Email string
			Groups              []string
		}
	}
	if json.Unmarshal(data, &me) != nil || resp.StatusCode != 200 || me.User.Username != subject {
		t.Fatal("invalid me response", resp.StatusCode)
	}
	return me.User.ID
}

func TestPortalLoginCanonicalHost(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "untrusted.example"} {
		req, err := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/portal/login?return_to=https://untrusted.example", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("X-Forwarded-Host", host)
		resp, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != f.cfg.Portal.BaseURL+"/portal/login" {
			t.Fatal("login did not redirect to the configured host", resp.StatusCode, resp.Header.Get("Location"))
		}
		if len(resp.Cookies()) != 0 {
			t.Fatal("login cookie issued on an alternate host")
		}
	}
	var pending int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM portal_login_attempts`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("alternate host created login attempts", pending, err)
	}
	// The canonical host still completes the full state, PKCE and session flow.
	b.login(t, "alice", []string{"staff"})
}

func TestPortalOIDCStateReplayAndTokenValidation(t *testing.T) {
	f := startPortal(t)
	for _, tc := range []struct {
		name  string
		alter func(map[string]any)
	}{
		{"issuer", func(c map[string]any) { c["iss"] = "http://wrong.example" }},
		{"audience", func(c map[string]any) { c["aud"] = "another-client" }},
		{"expiry", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }},
		{"nonce", func(c map[string]any) { c["nonce"] = "wrong" }},
		{"missing_nonce", func(c map[string]any) { delete(c, "nonce") }},
		{"authorized_party", func(c map[string]any) { c["azp"] = "another-client" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := f.browser(t)
			q, _ := b.challenge(t)
			code := f.provider.authorize(t, q, "alice", []string{"staff"}, tc.alter)
			resp, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
			if resp.StatusCode != 400 {
				t.Fatal(resp.StatusCode)
			}
		})
	}
	b := f.browser(t)
	q, cookie := b.challenge(t)
	code := f.provider.authorize(t, q, "alice", []string{"staff"}, nil)
	path := "/portal/callback?" + url.Values{"state": {q.Get("state")}, "code": {code}}.Encode()
	resp, _ := b.call(t, "GET", strings.Replace(path, q.Get("state"), "tampered", 1), "", "")
	if resp.StatusCode != 400 {
		t.Fatal("tampered state accepted")
	}
	other := f.browser(t)
	resp, _ = other.call(t, "GET", path, "", "")
	if resp.StatusCode != 400 {
		t.Fatal("callback accepted in a different browser")
	}
	resp, _ = b.call(t, "GET", path, "", "")
	if resp.StatusCode != 303 {
		t.Fatal("valid callback failed", resp.StatusCode)
	}
	u, _ := url.Parse(f.router.URL + "/portal/")
	b.client.Jar.SetCookies(u, []*http.Cookie{cookie})
	resp, _ = b.call(t, "GET", path, "", "")
	if resp.StatusCode != 400 {
		t.Fatal("replayed callback accepted")
	}
	q, cookie = b.challenge(t)
	code = f.provider.authorize(t, q, "alice", []string{"staff"}, nil)
	hash := sha256.Sum256([]byte(cookie.Value))
	execSQL(t, f.pool, `UPDATE portal_login_attempts SET expires=clock_timestamp()-interval '1 second' WHERE id_hash=$1`, hash[:])
	resp, _ = b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != 400 {
		t.Fatal("expired login challenge accepted")
	}
}

func TestPortalUserIsolationRevocationAndLogout(t *testing.T) {
	f := startPortal(t)
	a, b := f.browser(t), f.browser(t)
	for _, path := range []string{"me", "keys", "usage", "models"} {
		resp, _ := a.call(t, "GET", "/portal/api/"+path, "", "")
		if resp.StatusCode != 401 {
			t.Fatal(path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/portal/api", "/portal/api/unknown"} {
		resp, _ := a.call(t, "GET", path, "", "")
		if resp.StatusCode != 401 {
			t.Fatal("API fallback bypassed authentication")
		}
	}
	alice, bob := a.login(t, "alice", []string{"staff"}), b.login(t, "bob", []string{"students"})
	if alice == bob || alice == f.owner || bob == f.owner {
		t.Fatal("users linked by email instead of issuer and subject")
	}
	for _, expiry := range []string{"0", "-1", "36501", "1.5", `"7"`} {
		resp, _ := a.call(t, "POST", "/portal/api/keys", `{"label":"invalid expiry","expires_in_days":`+expiry+`}`, f.cfg.Portal.BaseURL)
		if resp.StatusCode != 400 {
			t.Fatal("invalid expiry accepted")
		}
	}
	resp, data := a.call(t, "POST", "/portal/api/keys", `{"label":"laptop","expires_in_days":7}`, f.cfg.Portal.BaseURL)
	var created struct{ Key string }
	if json.Unmarshal(data, &created) != nil || resp.StatusCode != 201 || !strings.HasPrefix(created.Key, "olr-") {
		t.Fatal("key was not issued")
	}
	_, data = a.call(t, "GET", "/portal/api/keys", "", "")
	var listed struct{ Keys []auth.KeyInfo }
	if json.Unmarshal(data, &listed) != nil || len(listed.Keys) != 1 || strings.Contains(string(data), created.Key) {
		t.Fatal("invalid key metadata")
	}
	id := listed.Keys[0].ID
	if listed.Keys[0].Expires == nil || time.Until(*listed.Keys[0].Expires) < 7*24*time.Hour-time.Minute || time.Until(*listed.Keys[0].Expires) > 7*24*time.Hour+time.Minute {
		t.Fatal("custom key expiry was not applied")
	}
	_, data = b.call(t, "GET", "/portal/api/keys", "", "")
	if strings.Contains(string(data), id) {
		t.Fatal("another user's key was listed")
	}
	resp, _ = b.call(t, "DELETE", "/portal/api/keys/"+id, `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 404 {
		t.Fatal("another user's key revoked", resp.StatusCode)
	}
	for _, origin := range []string{"https://evil.example", "null", "http://sibling.example.test", ""} {
		for _, method := range []string{"POST", "DELETE"} {
			path, body := "/portal/api/keys", `{"label":"evil"}`
			if method == "DELETE" {
				path += "/" + id
				body = `{}`
			}
			resp, _ := a.call(t, method, path, body, origin)
			if resp.StatusCode != 403 {
				t.Fatal("cross-origin mutation accepted", origin, resp.StatusCode)
			}
		}
	}
	// Warm the API-key cache, then revoke through the portal. The next request
	// must fail without polling or waiting for the authentication cache TTL.
	request := func() int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+created.Key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if request() != 200 {
		t.Fatal("created key did not authenticate")
	}
	resp, _ = a.call(t, "DELETE", "/portal/api/keys/"+id, `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 204 || request() != 401 {
		t.Fatal("revoked key remained usable")
	}
	resp, _ = a.call(t, "GET", "/v1/models", "", "")
	if resp.StatusCode != 401 {
		t.Fatal("session authorized inference")
	}
	// Force the cookie onto /v1 even though its browser path excludes it.
	portalURL, _ := url.Parse(f.router.URL + "/portal/")
	forced, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
	for _, cookie := range a.client.Jar.Cookies(portalURL) {
		forced.AddCookie(cookie)
	}
	forcedResponse, err := http.DefaultClient.Do(forced)
	if err != nil {
		t.Fatal(err)
	}
	forcedResponse.Body.Close()
	if forcedResponse.StatusCode != 401 || len(forcedResponse.Cookies()) != 0 {
		t.Fatal("API namespace read or set session cookies")
	}
	resp, data = a.call(t, "POST", "/portal/logout", `{}`, f.cfg.Portal.BaseURL)
	var logout struct {
		URL string `json:"logout_url"`
	}
	if json.Unmarshal(data, &logout) != nil || resp.StatusCode != 200 || !strings.HasPrefix(logout.URL, f.provider.issuer+"/logout?") || strings.Contains(logout.URL, "id_token_hint") {
		t.Fatal("invalid logout")
	}
	providerLogout, err := url.Parse(logout.URL)
	if err != nil || providerLogout.Query().Get("post_logout_redirect_uri") != f.cfg.Portal.BaseURL+"/" {
		t.Fatal("SSO logout did not return to the public homepage")
	}
	resp, _ = a.call(t, "GET", "/portal/api/me", "", "")
	if resp.StatusCode != 401 {
		t.Fatal("logged-out session survived")
	}
	var sessions int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE user_id=$1`, alice).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("logout failed to invalidate database session", err)
	}
	resp, _ = b.call(t, "GET", "/portal/api/me", "", "")
	if resp.StatusCode != 200 {
		t.Fatal("logout affected another user")
	}
}

func TestPortalRefreshGroupsDisableAndSessionExpiry(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	id := b.login(t, "alice", []string{"staff"})
	_, keyBody := b.call(t, "POST", "/portal/api/keys", `{"label":"group-refresh"}`, f.cfg.Portal.BaseURL)
	var created struct{ Key string }
	if json.Unmarshal(keyBody, &created) != nil || created.Key == "" {
		t.Fatal("key creation failed")
	}
	apiModels := func() int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+created.Key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var models struct{ Data []any }
		if json.NewDecoder(resp.Body).Decode(&models) != nil || resp.StatusCode != 200 {
			t.Fatal("API model listing failed")
		}
		return len(models.Data)
	}
	if apiModels() != 2 {
		t.Fatal("initial group access missing")
	}
	execSQL(t, f.pool, `UPDATE users SET synced_at='2000-01-01' WHERE id=$1`, id)
	if next := b.login(t, "alice", []string{"unknown"}); next != id {
		t.Fatal("relogin changed identity")
	}
	if apiModels() != 0 {
		t.Fatal("API key cache retained obsolete group access")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE user_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatal("relogin did not rotate its session", err)
	}
	_, data := b.call(t, "GET", "/portal/api/models", "", "")
	var models struct{ Models []any }
	if json.Unmarshal(data, &models) != nil || len(models.Models) != 0 {
		t.Fatal("group change did not affect model access")
	}
	var groups []string
	var synced time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT groups,synced_at FROM users WHERE id=$1`, id).Scan(&groups, &synced); err != nil || len(groups) != 1 || groups[0] != "unknown" || time.Since(synced) > time.Minute {
		t.Fatal("claims did not refresh", err)
	}
	execSQL(t, f.pool, `UPDATE sessions SET expires=clock_timestamp()-interval '1 second' WHERE user_id=$1`, id)
	resp, _ := b.call(t, "GET", "/portal/api/me", "", "")
	if resp.StatusCode != 401 {
		t.Fatal("expired session accepted")
	}
	b.login(t, "alice", []string{"unknown"})
	execSQL(t, f.pool, `UPDATE users SET enabled=false WHERE id=$1`, id)
	resp, _ = b.call(t, "GET", "/portal/api/me", "", "")
	if resp.StatusCode != 401 {
		t.Fatal("disabled user retained a session")
	}
	q, _ := b.challenge(t)
	code := f.provider.authorize(t, q, "alice", []string{"staff"}, nil)
	resp, _ = b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != 403 {
		t.Fatal("disabled user signed in", resp.StatusCode)
	}
}

func TestPortalUsagePeriodsAndIsolation(t *testing.T) {
	location, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 5, 12, 0, 0, 0, location)
	f := startPortal(t, usageOptions{location: location, clock: func() time.Time { return now }})
	a, b := f.browser(t), f.browser(t)
	alice, bob := a.login(t, "alice", []string{"staff"}), b.login(t, "bob", []string{"students"})
	for _, record := range []usage.Record{
		{UserID: alice, Model: "chat-model", StartedAt: now, InputTokens: 3, OutputTokens: 7, TTFBMS: limitPtr(int64(50)), LatencyMS: 400, IsStream: true},
		{UserID: alice, Model: "chat-model", StartedAt: now, InputTokens: 2, OutputTokens: 8, TTFBMS: limitPtr(int64(100)), LatencyMS: 101},
		{UserID: alice, Model: "chat-model", StartedAt: now.AddDate(0, 0, -1), InputTokens: 5, OutputTokens: 5, TTFBMS: limitPtr(int64(100)), LatencyMS: 600},
		{UserID: alice, Model: "last-month", StartedAt: now.AddDate(0, -1, 0), InputTokens: 999, OutputTokens: 999},
		{UserID: bob, Model: "private", StartedAt: now, InputTokens: 999, OutputTokens: 999},
	} {
		record.Protocol = "openai"
		record.Status = "200"
		f.writer.Record(record)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == 5 })
	resp, data := a.call(t, "GET", "/portal/api/usage", "", "")
	var result struct {
		Today, Month []struct {
			Model    string
			Requests int
			Input    int     `json:"input_tokens"`
			Output   int     `json:"output_tokens"`
			TTFB     float64 `json:"average_ttfb_ms"`
			TPS      float64 `json:"average_tokens_per_second"`
		}
		Quota struct {
			Used struct {
				Today int `json:"tokens_today"`
				Month int `json:"tokens_this_month"`
			}
		}
	}
	if json.Unmarshal(data, &result) != nil || resp.StatusCode != 200 || len(result.Today) != 1 || len(result.Month) != 1 || result.Today[0].Requests != 2 || result.Month[0].Requests != 3 || result.Today[0].Input != 5 || result.Today[0].Output != 15 || result.Month[0].Input != 10 || result.Month[0].Output != 20 || result.Today[0].TTFB != 75 || result.Today[0].TPS != 20 || result.Month[0].TPS != 20 || result.Quota.Used.Today != 20 || result.Quota.Used.Month != 30 {
		t.Fatal("incorrect scoped usage aggregates", resp.StatusCode)
	}
}

func TestPortalCustomClaimNames(t *testing.T) {
	f := startPortal(t)
	f.cfg.OIDC.UsernameClaim = "custom_name"
	f.cfg.OIDC.GroupsClaim = "memberships"
	b := f.browser(t)
	q, _ := b.challenge(t)
	code := f.provider.authorize(t, q, "subject", []string{"admins"}, func(c map[string]any) { c["custom_name"] = "custom-user"; c["memberships"] = []string{"students"} })
	resp, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if resp.StatusCode != 303 {
		t.Fatal("custom claims login failed", resp.StatusCode)
	}
	_, data := b.call(t, "GET", "/portal/api/me", "", "")
	var me struct {
		User struct {
			Username string
			Groups   []string
		}
	}
	if json.Unmarshal(data, &me) != nil || me.User.Username != "custom-user" || len(me.User.Groups) != 1 || me.User.Groups[0] != "students" {
		t.Fatal("configured claims were ignored")
	}
}

// The login route is public; it must not let anyone fill the database.
func TestPortalPendingLoginsAreCapped(t *testing.T) {
	previous := portal.MaxPendingLogins
	portal.MaxPendingLogins = 3
	t.Cleanup(func() { portal.MaxPendingLogins = previous })
	f := startPortal(t)
	for range 3 {
		f.browser(t).challenge(t)
	}
	resp, _ := f.browser(t).call(t, "GET", "/portal/login", "", "")
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatal("login beyond the cap", resp.StatusCode)
	}
	var pending int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM portal_login_attempts`).Scan(&pending); err != nil || pending != 3 {
		t.Fatal("pending logins", pending, err)
	}
	// Expired attempts free capacity again.
	if _, err := f.pool.Exec(t.Context(), `UPDATE portal_login_attempts SET expires=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	f.browser(t).login(t, "after-cap", []string{"staff"})
}

func TestPortalPrivacyDeletion(t *testing.T) {
	f := startPortal(t)
	a, b := f.browser(t), f.browser(t)
	alice, bob := a.login(t, "alice", []string{"staff"}), b.login(t, "bob", []string{"staff"})
	resp, data := a.call(t, "POST", "/portal/api/keys", `{"label":"laptop"}`, f.cfg.Portal.BaseURL)
	var created struct{ Key string }
	if json.Unmarshal(data, &created) != nil || resp.StatusCode != 201 {
		t.Fatal("key was not issued")
	}
	for range 2 {
		req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+created.Key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatal("inference failed", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	f.writer.Record(usage.Record{UserID: bob, Model: "chat-model", Protocol: "openai", Status: "200", StartedAt: time.Now(), InputTokens: 1, OutputTokens: 1})
	waitFor(t, func() bool { return f.writer.Counters().Written == 3 })
	// A call that bob's forwarder key made for alice is alice's record.
	execSQL(t, f.pool, `INSERT INTO usage (user_id, forwarded_for, model, replica, protocol, status, latency_ms) VALUES ($1, $2, 'chat-model', 'r', 'openai', '200', 1)`, bob, alice)
	quota := func(br *portalBrowser) int {
		_, data := br.call(t, "GET", "/portal/api/usage", "", "")
		var result struct {
			Quota struct {
				Used struct {
					Today int `json:"tokens_today"`
				}
			}
		}
		if json.Unmarshal(data, &result) != nil {
			t.Fatal("invalid usage")
		}
		return result.Quota.Used.Today
	}
	before := quota(a)
	if before == 0 {
		t.Fatal("no usage charged")
	}
	for _, origin := range []string{"https://evil.example", ""} {
		if resp, _ := a.call(t, "DELETE", "/portal/api/usage", `{}`, origin); resp.StatusCode != 403 {
			t.Fatal("cross-origin deletion accepted", resp.StatusCode)
		}
	}
	resp, data = a.call(t, "DELETE", "/portal/api/usage", `{}`, f.cfg.Portal.BaseURL)
	if resp.StatusCode != 200 || string(data) != "{\"deleted\":3}\n" {
		t.Fatalf("delete activity: %d %s", resp.StatusCode, data)
	}
	count := func(sql string, args ...any) int {
		var n int
		if err := f.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM usage WHERE forwarded_for=$1 OR (user_id=$1 AND forwarded_for IS NULL)`, alice); n != 0 {
		t.Fatal("alice's records remain", n)
	}
	if n := count(`SELECT count(*) FROM usage WHERE user_id=$1 AND forwarded_for IS NULL`, bob); n != 1 {
		t.Fatal("bob's records were touched", n)
	}
	if quota(a) != before || count(`SELECT count(*) FROM usage_daily WHERE user_id=$1`, alice) != 1 {
		t.Fatal("deleting activity changed the quota")
	}

	// Revoke, then remove the key for good; its usage rows lose the reference.
	_, data = a.call(t, "GET", "/portal/api/keys", "", "")
	var listed struct{ Keys []auth.KeyInfo }
	if json.Unmarshal(data, &listed) != nil || len(listed.Keys) != 1 {
		t.Fatal("key list")
	}
	id := listed.Keys[0].ID
	f.writer.Record(usage.Record{UserID: alice, KeyID: id, Model: "chat-model", Protocol: "openai", Status: "200", StartedAt: time.Now()})
	waitFor(t, func() bool { return f.writer.Counters().Written == 4 })
	if resp, _ := b.call(t, "DELETE", "/portal/api/keys/"+id, `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 404 {
		t.Fatal("another user removed the key")
	}
	if resp, _ := a.call(t, "DELETE", "/portal/api/keys/"+id, `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 204 || count(`SELECT count(*) FROM api_keys WHERE id=$1 AND revoked`, id) != 1 {
		t.Fatal("first delete must revoke")
	}
	if resp, _ := a.call(t, "DELETE", "/portal/api/keys/"+id, `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 204 {
		t.Fatal("second delete failed")
	}
	_, data = a.call(t, "GET", "/portal/api/keys", "", "")
	if strings.Contains(string(data), id) || count(`SELECT count(*) FROM api_keys WHERE id=$1`, id) != 0 || count(`SELECT count(*) FROM usage WHERE key_id=$1`, id) != 0 || count(`SELECT count(*) FROM usage WHERE user_id=$1 AND key_id IS NULL`, alice) != 1 {
		t.Fatal("removed key still referenced")
	}
	if resp, _ := a.call(t, "DELETE", "/portal/api/keys/"+id, `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 404 {
		t.Fatal("removed key still found")
	}

	// Operator erasure leaves no row that references the user.
	execSQL(t, f.pool, `INSERT INTO usage (user_id, forwarded_for, model, replica, protocol, status, latency_ms) VALUES ($1, $2, 'chat-model', 'r', 'openai', '200', 1)`, bob, alice)
	if err := (auth.Admin{Pool: f.pool}).DeleteUser(t.Context(), alice); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`SELECT count(*) FROM users WHERE id=$1`, `SELECT count(*) FROM api_keys WHERE user_id=$1`,
		`SELECT count(*) FROM sessions WHERE user_id=$1`, `SELECT count(*) FROM usage WHERE user_id=$1 OR forwarded_for=$1`,
		`SELECT count(*) FROM usage_daily WHERE user_id=$1`,
	} {
		if n := count(sql, alice); n != 0 {
			t.Fatal("erased user still referenced:", sql, n)
		}
	}
	if resp, _ := a.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("erased user's session survived")
	}
	if resp, _ := b.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 200 || count(`SELECT count(*) FROM usage WHERE user_id=$1`, bob) != 1 {
		t.Fatal("erasure affected another user")
	}
	if errors.Is((auth.Admin{Pool: f.pool}).DeleteUser(t.Context(), alice), auth.ErrNotFound) == false {
		t.Fatal("deleting a missing user must report not found")
	}
}

func TestPortalRequestsDailyChartAndAdmin(t *testing.T) {
	f := startPortal(t, usageOptions{group: &config.Group{Models: []string{"*"}}})
	a, b, root := f.browser(t), f.browser(t), f.browser(t)
	alice, bob := a.login(t, "alice", []string{"staff"}), b.login(t, "bob", []string{"staff"})
	root.login(t, "root", []string{"admins"})
	now := time.Now()
	for i, record := range []usage.Record{
		{UserID: alice, Model: "chat-model", StartedAt: now.Add(-time.Minute), InputTokens: 3, OutputTokens: 7, LatencyMS: 40, TTFBMS: limitPtr(int64(5)), IsStream: true},
		{UserID: alice, Model: "chat-model", StartedAt: now.AddDate(0, 0, -3), InputTokens: 1, OutputTokens: 1, LatencyMS: 10},
		{UserID: bob, Model: "bob-private", StartedAt: now, InputTokens: 9, OutputTokens: 9, LatencyMS: 10},
	} {
		record.Protocol, record.Status = "openai", "200"
		f.writer.Record(record)
		waitFor(t, func() bool { return f.writer.Counters().Written == uint64(i+1) })
	}

	resp, data := a.call(t, "GET", "/portal/api/requests", "", "")
	var requests struct {
		Requests []struct {
			Time         time.Time
			Model        string
			Status       string
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			TTFB         *int64 `json:"ttfb_ms"`
		}
		Limit int
	}
	if json.Unmarshal(data, &requests) != nil || resp.StatusCode != 200 || requests.Limit != 200 || len(requests.Requests) != 2 ||
		requests.Requests[0].InputTokens != 3 || *requests.Requests[0].TTFB != 5 || !requests.Requests[0].Time.After(requests.Requests[1].Time) || strings.Contains(string(data), "bob-private") {
		t.Fatalf("requests: %d %s", resp.StatusCode, data)
	}
	resp, data = a.call(t, "GET", "/portal/api/usage/daily", "", "")
	var daily struct {
		Days []struct {
			Day          string
			Requests     int64
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		}
	}
	if json.Unmarshal(data, &daily) != nil || resp.StatusCode != 200 || len(daily.Days) != 30 || daily.Days[29].Day != now.UTC().Format("2006-01-02") ||
		daily.Days[29].Requests != 1 || daily.Days[26].InputTokens != 1 || daily.Days[0].Requests != 0 {
		t.Fatalf("daily: %d %s", resp.StatusCode, data)
	}

	// Admin pages are refused for users outside admin groups.
	for _, path := range []string{"/portal/api/admin/users", "/portal/api/admin/users/" + bob, "/portal/api/admin/groups", "/portal/api/admin/replicas"} {
		if resp, _ := a.call(t, "GET", path, "", ""); resp.StatusCode != 403 {
			t.Fatal("non-admin read", path, resp.StatusCode)
		}
		if resp, _ := f.browser(t).call(t, "GET", path, "", ""); resp.StatusCode != 401 {
			t.Fatal("anonymous read", path, resp.StatusCode)
		}
	}
	if resp, _ := a.call(t, "PUT", "/portal/api/admin/users/"+bob+"/overrides/rpm", `{"value":1}`, f.cfg.Portal.BaseURL); resp.StatusCode != 403 {
		t.Fatal("non-admin override", resp.StatusCode)
	}
	if resp, _ := a.call(t, "PUT", "/portal/api/admin/users/"+bob+"/enabled", `{"enabled":false}`, f.cfg.Portal.BaseURL); resp.StatusCode != 403 {
		t.Fatal("non-admin disable", resp.StatusCode)
	}
	_, data = root.call(t, "GET", "/portal/api/me", "", "")
	if !strings.Contains(string(data), `"admin":true`) {
		t.Fatal("admin flag", string(data))
	}
	resp, data = root.call(t, "GET", "/portal/api/admin/users?match=ALI", "", "")
	var users struct {
		Users []struct {
			ID          string
			Username    string
			TokensToday int64 `json:"tokens_today"`
		}
	}
	if json.Unmarshal(data, &users) != nil || resp.StatusCode != 200 || len(users.Users) != 1 || users.Users[0].ID != alice || users.Users[0].TokensToday != 10 {
		t.Fatalf("admin users: %d %s", resp.StatusCode, data)
	}
	// Overrides through the portal, with origin checks and audit fields.
	if resp, _ := root.call(t, "PUT", "/portal/api/admin/users/"+alice+"/overrides/rpm", `{"value":1}`, "https://evil.example"); resp.StatusCode != 403 {
		t.Fatal("cross-origin override", resp.StatusCode)
	}
	for _, body := range []string{`{"value":0}`, `{"value":-5}`, `{"value":1,"extra":1}`} {
		if resp, _ := root.call(t, "PUT", "/portal/api/admin/users/"+alice+"/overrides/rpm", body, f.cfg.Portal.BaseURL); resp.StatusCode != 400 {
			t.Fatal("invalid override accepted", body, resp.StatusCode)
		}
	}
	if resp, _ := root.call(t, "PUT", "/portal/api/admin/users/"+alice+"/overrides/bogus", `{"value":1}`, f.cfg.Portal.BaseURL); resp.StatusCode != 400 {
		t.Fatal("unknown field accepted", resp.StatusCode)
	}
	if resp, _ := root.call(t, "PUT", "/portal/api/admin/users/"+alice+"/overrides/tokens_per_day", `{"value":12345,"reason":"exam week"}`, f.cfg.Portal.BaseURL); resp.StatusCode != 204 {
		t.Fatal("override", resp.StatusCode)
	}
	_, data = a.call(t, "GET", "/portal/api/me", "", "")
	if !strings.Contains(string(data), `"tokens_per_day":12345`) {
		t.Fatal("override not applied", string(data))
	}
	_, data = root.call(t, "GET", "/portal/api/admin/users/"+alice, "", "")
	if !strings.Contains(string(data), `"set_by":"portal:root"`) || !strings.Contains(string(data), `"reason":"exam week"`) {
		t.Fatal("audit fields", string(data))
	}
	if resp, _ := root.call(t, "DELETE", "/portal/api/admin/users/"+alice+"/overrides/tokens_per_day", `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 204 {
		t.Fatal("clear override", resp.StatusCode)
	}
	// Disabling a user ends their session at once; admins cannot lock themselves out.
	if resp, _ := root.call(t, "PUT", "/portal/api/admin/users/"+bob+"/enabled", `{"enabled":false}`, f.cfg.Portal.BaseURL); resp.StatusCode != 204 {
		t.Fatal("disable", resp.StatusCode)
	}
	if resp, _ := b.call(t, "GET", "/portal/api/me", "", ""); resp.StatusCode != 401 {
		t.Fatal("disabled user kept a session")
	}
	_, data = root.call(t, "GET", "/portal/api/me", "", "")
	var me struct{ User struct{ ID string } }
	_ = json.Unmarshal(data, &me)
	if resp, _ := root.call(t, "PUT", "/portal/api/admin/users/"+me.User.ID+"/enabled", `{"enabled":false}`, f.cfg.Portal.BaseURL); resp.StatusCode != 400 {
		t.Fatal("self-disable", resp.StatusCode)
	}
	resp, data = root.call(t, "GET", "/portal/api/admin/groups", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(data), `{"group":"staff","members":2,"requests_today":2,"tokens_today":28`) {
		t.Fatalf("groups: %s", data)
	}
	resp, data = root.call(t, "GET", "/portal/api/admin/replicas", "", "")
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"model":"chat-model"`) || !strings.Contains(string(data), `"healthy":true`) {
		t.Fatalf("replicas: %s", data)
	}
}

func TestPortalRequiredClaim(t *testing.T) {
	f := startPortal(t)
	f.cfg.OIDC.RequiredClaim = &config.RequiredClaim{Name: "affiliation", Value: "member@example.org"}
	attempt := func(subject string, alter func(map[string]any)) int {
		b := f.browser(t)
		q, _ := b.challenge(t)
		code := f.provider.authorize(t, q, subject, []string{"staff"}, alter)
		resp, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
		return resp.StatusCode
	}
	for name, tc := range map[string]struct {
		claim any
		want  int
	}{
		"string":        {"member@example.org", 303},
		"array":         {[]string{"staff@example.org", "member@example.org"}, 303},
		"other value":   {"staff@example.org", 403},
		"array without": {[]string{"staff@example.org"}, 403},
		"case differs":  {"Member@example.org", 403},
		"not a string":  {map[string]string{"v": "member@example.org"}, 403},
		"missing (nil)": {nil, 403},
	} {
		alter := func(c map[string]any) {
			if tc.claim != nil {
				c["affiliation"] = tc.claim
			}
		}
		if got := attempt("claim-"+strings.ReplaceAll(name, " ", "-"), alter); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
	var refused int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE sub IN ('claim-other-value','claim-missing-(nil)')`).Scan(&refused); err != nil || refused != 0 {
		t.Fatal("a refused login created a user", refused, err)
	}
}
