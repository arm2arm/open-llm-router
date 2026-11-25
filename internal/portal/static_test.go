package portal

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
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

func TestPortalAssetsAreCompressedAndCachedByContentHash(t *testing.T) {
	script := []byte(strings.Repeat("console.log('portal bundle');\n", 200))
	assets := fstest.MapFS{
		"dist/index.html":                 {Data: []byte("<!doctype html><title>Portal</title><script src=/portal/assets/index-CXpicPk9.js>")},
		"dist/assets/index-CXpicPk9.js":   {Data: script},
		"dist/assets/unhashed.js":         {Data: script},
		"dist/people/avatar.png":          {Data: []byte("\x89PNG\r\n\x1a\n")},
		"dist/.keep":                      {},
		"dist/assets/.hidden-CXpicPk9.js": {Data: script},
	}
	server := newAssetServer(assets)
	get := func(path string, header http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		for key, values := range header {
			r.Header[key] = values
		}
		w := httptest.NewRecorder()
		server.serve(w, r)
		return w
	}
	plain := get("/portal/assets/index-CXpicPk9.js", nil)
	if plain.Code != 200 || plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(plain.Body.Bytes(), script) {
		t.Fatal("identity bundle", plain.Code, plain.Header())
	}
	if plain.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("hashed bundle is not cacheable", plain.Header())
	}
	compressed := get("/portal/assets/index-CXpicPk9.js", http.Header{"Accept-Encoding": {"br, gzip;q=0.8"}})
	if compressed.Header().Get("Content-Encoding") != "gzip" || compressed.Body.Len() >= len(script)/2 || !strings.HasPrefix(compressed.Header().Get("Content-Type"), "text/javascript") {
		t.Fatal("bundle was not compressed", compressed.Header(), compressed.Body.Len())
	}
	if compressed.Header().Get("ETag") == plain.Header().Get("ETag") || plain.Header().Get("ETag") == "" {
		t.Fatal("representations share a validator")
	}
	reader, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := io.ReadAll(reader); err != nil || !bytes.Equal(decoded, script) {
		t.Fatal("compressed bundle differs")
	}
	if refused := get("/portal/assets/index-CXpicPk9.js", http.Header{"Accept-Encoding": {"gzip;q=0"}}); refused.Header().Get("Content-Encoding") != "" {
		t.Fatal("gzip sent despite q=0")
	}
	revalidated := get("/portal/assets/index-CXpicPk9.js", http.Header{"Accept-Encoding": {"gzip"}, "If-None-Match": {compressed.Header().Get("ETag")}})
	if revalidated.Code != http.StatusNotModified || revalidated.Body.Len() != 0 {
		t.Fatal("matching validator was not answered with 304", revalidated.Code)
	}
	// Only content-hashed bundle names are immutable; other files revalidate,
	// and the application shell keeps the portal's no-store policy.
	for path, want := range map[string]string{"/portal/assets/unhashed.js": "no-cache", "/portal/people/avatar.png": "no-cache", "/portal/": "", "/portal/keys": ""} {
		if got := get(path, nil); got.Code != 200 || got.Header().Get("Cache-Control") != want {
			t.Fatalf("%s: %d cache %q, want %q", path, got.Code, got.Header().Get("Cache-Control"), want)
		}
	}
	for _, path := range []string{"/portal/.keep", "/portal/assets/.hidden-CXpicPk9.js", "/portal/../web/assets.go"} {
		if got := get(path, nil); got.Code != 404 {
			t.Fatalf("%s: %d", path, got.Code)
		}
	}
}
