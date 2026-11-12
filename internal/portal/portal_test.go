package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestMutationOriginMetadataAndJSON(t *testing.T) {
	h := &Handler{cfg: &config.Config{Portal: config.Portal{BaseURL: "https://portal.example"}}}
	for _, tc := range []struct {
		origin, site, media, body string
		want                      int
	}{
		{"https://portal.example", "same-origin", "application/json", `{}`, 200},
		{"https://portal.example", "", "application/json; charset=utf-8", `{}`, 200},
		{"https://evil.example", "cross-site", "application/json", `{}`, 403},
		{"https://portal.example", "cross-site", "application/json", `{}`, 403},
		{"https://portal.example", "same-site", "application/json", `{}`, 403},
		{"", "same-origin", "application/json", `{}`, 403},
		{"null", "", "application/json", `{}`, 403},
		{"https://portal.example", "", "text/plain", `{}`, 400},
		{"https://portal.example", "", "application/json", `null`, 400},
		{"https://portal.example", "", "application/json", `[]`, 400},
		{"https://portal.example", "", "application/json", `{} {}`, 400},
		{"https://portal.example", "", "application/json", `{"unexpected":true}`, 400},
		{"https://portal.example", "", "application/json", `{"value":"` + strings.Repeat("x", 9000) + `"}`, 400},
	} {
		r := httptest.NewRequest("POST", "https://portal.example/portal/logout", strings.NewReader(tc.body))
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.Header.Set("Content-Type", tc.media)
		w := httptest.NewRecorder()
		var body struct{}
		ok := h.jsonBody(w, r, &body)
		if ok != (tc.want == 200) || (!ok && w.Code != tc.want) {
			t.Fatalf("origin=%q site=%q media=%q status=%d", tc.origin, tc.site, tc.media, w.Code)
		}
	}
}

func TestSessionCookieAndOpaqueID(t *testing.T) {
	h := &Handler{cfg: &config.Config{Portal: config.Portal{CookieSecure: true}}}
	w := httptest.NewRecorder()
	id := randomToken()
	h.cookie(w, sessionCookie, id, 8*time.Hour)
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/portal" || c.Domain != "" || c.MaxAge != 28800 {
		t.Fatal("unsafe session cookie")
	}
	r := httptest.NewRequest("GET", "https://portal.example/portal/api/me", nil)
	r.AddCookie(c)
	if _, ok := cookieHash(r, sessionCookie); !ok {
		t.Fatal("valid ID rejected")
	}
	r.AddCookie(c)
	if _, ok := cookieHash(r, sessionCookie); ok {
		t.Fatal("duplicate session cookie accepted")
	}
}

func TestDiscoveryRefusesWrongIssuerAndInsecureDefault(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"https://wrong.example"}`))
	}))
	defer provider.Close()
	cfg := &config.Config{OIDC: &config.OIDC{Issuer: provider.URL, PortalClientID: "client"}, OIDCClientSecret: "secret", Portal: config.Portal{BaseURL: "http://portal.example", CookieSecure: true}}
	h, err := New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://portal.example/portal/login", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "secure public origin") || len(w.Result().Cookies()) != 0 {
		t.Fatal("HTTP cookie login accepted with secure cookies by default")
	}
	// Replace the config only after closing the first handler's background job.
	h.Close()
	cfg.Portal.CookieSecure = false
	h, err = New(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatal("discovery blocked local startup", err)
	}
	defer h.Close()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://portal.example/portal/api/auth", nil))
	var methods struct {
		Local struct{ Available bool }
		OIDC  struct{ Configured, Available bool }
	}
	if json.Unmarshal(w.Body.Bytes(), &methods) != nil || !methods.Local.Available || !methods.OIDC.Configured || methods.OIDC.Available {
		t.Fatal("wrong discovery issuer accepted or local authentication disabled")
	}
}

func TestLocalAuthenticationRejectsCrossOriginBeforePasswords(t *testing.T) {
	h, err := New(t.Context(), &config.Config{Portal: config.Portal{BaseURL: "https://portal.example", CookieSecure: true}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, origin := range []string{"", "null", "https://evil.example"} {
		r := httptest.NewRequest("POST", "https://portal.example/portal/login/local", strings.NewReader(`{"username":"admin","password":"private password"}`))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || len(w.Result().Cookies()) != 0 {
			t.Fatal("cross-origin password login accepted")
		}
	}
}

func TestPasswordWorkIsBoundedAndForwardingHeadersIgnored(t *testing.T) {
	h := &Handler{passwordSlots: make(chan struct{}, 2)}
	h.passwordSlots <- struct{}{}
	h.passwordSlots <- struct{}{}
	r := httptest.NewRequest("POST", "https://portal.example/portal/login/local", nil)
	r.RemoteAddr = "[::ffff:192.0.2.1]:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	r.Header.Set("Forwarded", "for=198.51.100.2")
	if got := clientAddress(r); got != "192.0.2.1" {
		t.Fatal("untrusted address used for throttling", got)
	}
	w := httptest.NewRecorder()
	if h.allowPassword(w, r, "admin") || w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("unbounded concurrent password work")
	}
	if len(h.passwordSlots) != 2 {
		t.Fatal("rejected request leaked a password slot")
	}
}
