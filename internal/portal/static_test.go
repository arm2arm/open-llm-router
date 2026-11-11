package portal

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPortalAssetsAndFallback(t *testing.T) {
	assets := fstest.MapFS{"dist/index.html": {Data: []byte("<!doctype html><title>Portal</title>")}, "dist/assets/app.js": {Data: []byte("console.log('portal')")}, "dist/.keep": {}}
	for _, tc := range []struct {
		method, path string
		status       int
		contains     string
	}{
		{"GET", "/portal/", 200, "<title>Portal"},
		{"GET", "/portal/keys", 200, "<title>Portal"},
		{"GET", "/portal/usage", 200, "<title>Portal"},
		{"GET", "/portal/assets/app.js", 200, "console.log"},
		{"GET", "/portal/assets/missing.js", 404, ""},
		{"GET", "/portal/.keep", 404, ""},
		{"POST", "/portal/keys", 405, ""},
		{"HEAD", "/portal/connect", 200, ""},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		serveAssets(w, r, assets)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.contains) {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if w.Code == 200 && !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self';") {
			t.Fatal("missing script CSP")
		}
	}
}
