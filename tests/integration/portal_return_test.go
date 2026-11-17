package integration_test

import (
	"net/url"
	"testing"
)

func TestPortalReauthenticationReturnPathIsBoundToLoginAttempt(t *testing.T) {
	f := startPortal(t)
	for _, tc := range []struct{ next, want string }{
		{"/portal/admin/advanced?draft=1#models", "/portal/admin/advanced?draft=1#models"},
		{"https://other.example/portal/keys", "/portal/"},
		{"//other.example/portal/keys", "/portal/"},
		{"/portal/../../outside", "/portal/"},
		{"/portal/login", "/portal/"},
	} {
		t.Run(tc.next, func(t *testing.T) {
			browser := f.browser(t)
			response, _ := browser.call(t, "GET", "/portal/login?"+url.Values{"next": {tc.next}}.Encode(), "", "")
			challenge, err := response.Location()
			if err != nil || response.StatusCode != 303 {
				t.Fatal("no login challenge", response.StatusCode, err)
			}
			code := f.provider.authorize(t, challenge.Query(), "returning-user", []string{"staff"}, nil)
			// The callback cannot replace the destination recorded with the original
			// browser-bound state, even when another next value is supplied.
			response, _ = browser.call(t, "GET", "/portal/callback?"+url.Values{"state": {challenge.Query().Get("state")}, "code": {code}, "next": {"https://other.example"}}.Encode(), "", "")
			if response.StatusCode != 303 || response.Header.Get("Location") != tc.want {
				t.Fatalf("unsafe or lost return path: %d %q", response.StatusCode, response.Header.Get("Location"))
			}
			execSQL(t, f.pool, `UPDATE sessions SET expires=clock_timestamp()-interval '1 second'`)
			response, _ = browser.call(t, "GET", "/portal/api/auth/session", "", "")
			if response.StatusCode != 401 {
				t.Fatalf("expired cookie accepted: %d", response.StatusCode)
			}
		})
	}
}
