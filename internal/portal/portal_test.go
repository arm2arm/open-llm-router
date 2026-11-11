package portal

import (
	"context"
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
	if _, err := New(context.Background(), cfg, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatal("HTTP accepted with secure cookies by default")
	}
	cfg.Portal.CookieSecure = false
	if _, err := New(context.Background(), cfg, nil, nil); err == nil || err.Error() != "portal OIDC discovery failed" {
		t.Fatal("wrong discovery issuer accepted", err)
	}
}
