// Package portal implements OIDC login and a session-authenticated JSON API.
package portal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

const sessionCookie = "olr_session"
const loginCookie = "olr_login"
const sessionLifetime = 8 * time.Hour

type Handler struct {
	cfg       *config.Config
	pool      *pgxpool.Pool
	limits    *limits.Manager
	oauth     oauth2.Config
	verifier  *oidc.IDTokenVerifier
	client    *http.Client
	transport *http.Transport
	logout    string
	mux       *http.ServeMux
}

func New(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, manager *limits.Manager) (*Handler, error) {
	if cfg.OIDC == nil || cfg.OIDC.Issuer == "" || cfg.OIDC.PortalClientID == "" || cfg.OIDCClientSecret == "" {
		return nil, errors.New("portal requires an OIDC issuer, client ID and client secret")
	}
	if cfg.Portal.CookieSecure && !strings.HasPrefix(cfg.Portal.BaseURL, "https://") {
		return nil, errors.New("PORTAL_BASE_URL must use HTTPS with secure cookies; local HTTP development must explicitly set PORTAL_COOKIE_SECURE=false")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	var rt http.RoundTripper = transport
	if cfg.OIDC.BackchannelURL != "" {
		public, err := url.Parse(cfg.OIDC.Issuer)
		if err != nil {
			return nil, errors.New("invalid issuer")
		}
		internal, err := url.Parse(cfg.OIDC.BackchannelURL)
		if err != nil {
			return nil, errors.New("invalid OIDC backchannel URL")
		}
		rt = backchannel{public, internal, transport}
	}
	client := &http.Client{Transport: rt, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.OIDC.Issuer)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, errors.New("portal OIDC discovery failed")
	}
	var metadata struct {
		Logout string `json:"end_session_endpoint"`
	}
	if provider.Claims(&metadata) != nil || !endpoint(metadata.Logout) {
		transport.CloseIdleConnections()
		return nil, errors.New("OIDC discovery must advertise an HTTP(S) end_session_endpoint")
	}
	h := &Handler{cfg: cfg, pool: pool, limits: manager, client: client, transport: transport, logout: metadata.Logout, mux: http.NewServeMux(),
		oauth:    oauth2.Config{ClientID: cfg.OIDC.PortalClientID, ClientSecret: cfg.OIDCClientSecret, RedirectURL: cfg.Portal.BaseURL + "/portal/callback", Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile", "email"}},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.OIDC.PortalClientID})}
	h.mux.HandleFunc("GET /portal/login", h.login)
	h.mux.HandleFunc("GET /portal/callback", h.callback)
	h.mux.HandleFunc("POST /portal/logout", h.authenticated(h.signout))
	h.mux.HandleFunc("GET /portal/api/me", h.authenticated(h.me))
	h.mux.HandleFunc("GET /portal/api/keys", h.authenticated(h.keys))
	h.mux.HandleFunc("POST /portal/api/keys", h.authenticated(h.createKey))
	h.mux.HandleFunc("DELETE /portal/api/keys/{id}", h.authenticated(h.revokeKey))
	h.mux.HandleFunc("GET /portal/api/usage", h.authenticated(h.usage))
	h.mux.HandleFunc("GET /portal/api/models", h.authenticated(h.models))
	h.mux.HandleFunc("/portal/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/portal/api" || strings.HasPrefix(r.URL.Path, "/portal/api/") {
			h.authenticated(func(w http.ResponseWriter, _ *http.Request, _ user) {
				fail(w, 404, "not_found_error", "route not found")
			})(w, r)
			return
		}
		staticPortal(w, r)
	})
	h.mux.HandleFunc("GET /portal", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/portal/", http.StatusSeeOther) })
	return h, nil
}

func (h *Handler) Close() { h.transport.CloseIdleConnections() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	h.mux.ServeHTTP(w, r.WithContext(ctx))
}

type backchannel struct {
	public, internal *url.URL
	next             http.RoundTripper
}

func (b backchannel) RoundTrip(r *http.Request) (*http.Response, error) {
	prefix := strings.TrimSuffix(b.public.Path, "/")
	if r.URL.Scheme == b.public.Scheme && r.URL.Host == b.public.Host && (r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/")) {
		r = r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = b.internal.Scheme, b.internal.Host
		u.Path = strings.TrimSuffix(b.internal.Path, "/") + strings.TrimPrefix(u.Path, prefix)
		u.RawPath = ""
		r.URL = &u
		r.Host = ""
	}
	return b.next.RoundTrip(r)
}

func endpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.Fragment == ""
}
func randomToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func cookieHash(r *http.Request, name string) ([32]byte, bool) {
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			value = c.Value
			count++
		}
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return sha256.Sum256([]byte(value)), count == 1 && err == nil && len(decoded) == 32
}
func (h *Handler) cookie(w http.ResponseWriter, name, value string, lifetime time.Duration) {
	maxAge := int(lifetime.Seconds())
	if lifetime < 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/portal", HttpOnly: true, Secure: h.cfg.Portal.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: time.Now().Add(lifetime)})
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, kind, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"type": kind, "message": message}})
}
func unavailable(w http.ResponseWriter) { fail(w, 503, "api_error", "portal temporarily unavailable") }

func (h *Handler) jsonBody(w http.ResponseWriter, r *http.Request, value any) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != h.cfg.Portal.BaseURL || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
		fail(w, 403, "permission_error", "request origin is forbidden")
		return false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 400, "invalid_request_error", "application/json is required")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		fail(w, 400, "invalid_request_error", "invalid JSON body")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		fail(w, 400, "invalid_request_error", "a single JSON object is required")
		return false
	}
	fields := json.NewDecoder(bytes.NewReader(raw))
	fields.DisallowUnknownFields()
	if fields.Decode(value) != nil {
		fail(w, 400, "invalid_request_error", "invalid JSON fields")
		return false
	}
	return true
}
