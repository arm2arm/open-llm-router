// Package home serves the public project overview and portal entry point.
package home

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"strings"
	"time"
)

//go:embed index.html
var pageHTML string

//go:embed home.css
var stylesheet string

var page = template.Must(template.New("home").Parse(pageHTML))

// New returns a public home page. The caller controls whether portal links appear.
func New(portalEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		if err := page.Execute(&body, struct{ PortalEnabled bool }{portalEnabled}); err != nil {
			http.Error(w, "home page unavailable", http.StatusInternalServerError)
			return
		}
		setHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(body.Bytes()))
	})
	mux.HandleFunc("GET /assets/home.css", func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		http.ServeContent(w, r, "home.css", time.Time{}, strings.NewReader(stylesheet))
	})
	return mux
}

func setHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
