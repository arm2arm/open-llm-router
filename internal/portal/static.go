package portal

import (
	"bytes"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/web"
)

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

func staticPortal(w http.ResponseWriter, r *http.Request) {
	serveAssets(w, r, web.Assets)
}

func serveAssets(w http.ResponseWriter, r *http.Request, assets fs.FS) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/portal/")
	name = path.Clean(name)
	if strings.HasPrefix(name, ".") && name != "." {
		http.NotFound(w, r)
		return
	}
	if name == "." {
		name = "index.html"
	}
	data, err := fs.ReadFile(assets, "dist/"+name)
	if err != nil && path.Ext(name) == "" {
		name = "index.html"
		data, err = fs.ReadFile(assets, "dist/"+name)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
