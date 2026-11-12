// Package portal implements local/OIDC login and a session-authenticated JSON API.
package portal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/jackc/pgx/v5/pgxpool"
)

const sessionCookie = "olr_session"
const loginCookie = "olr_login"
const sessionLifetime = 8 * time.Hour

type Handler struct {
	cfg           *config.Config
	pool          *pgxpool.Pool
	limits        *limits.Manager
	sso           atomic.Pointer[ssoProvider]
	ssoAvailable  atomic.Bool
	cancel        context.CancelFunc
	done          chan struct{}
	passwordSlots chan struct{}
	mux           *http.ServeMux
	// Pools is shown on the admin pages; installed before serving.
	Traces    *requesttrace.Service
	Pools     *pool.Manager
	Endpoints *endpoints.Service
	keyCipher *auth.KeyCipher
}

func New(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, manager *limits.Manager) (*Handler, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h := &Handler{cfg: cfg, pool: pool, limits: manager, mux: http.NewServeMux(), done: make(chan struct{}), passwordSlots: make(chan struct{}, 2)}
	var err error
	h.keyCipher, err = auth.LoadKeyCipher(os.Getenv)
	if err != nil {
		return nil, err
	}
	h.startSSO()
	h.mux.HandleFunc("GET /portal/login", h.login)
	h.mux.HandleFunc("GET /portal/login/local", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/portal/sign-in/local", http.StatusSeeOther)
	})
	h.mux.HandleFunc("GET /portal/sign-in/keycloak", h.browserLogin)
	h.mux.HandleFunc("POST /portal/login/local", h.localLogin)
	h.mux.HandleFunc("GET /portal/callback", h.callback)
	h.mux.HandleFunc("GET /portal/api/auth", h.authMethods)
	h.mux.HandleFunc("GET /portal/api/auth/session", h.withSession(true, func(w http.ResponseWriter, r *http.Request, u user) {
		respond(w, 200, map[string]any{"auth_method": u.AuthMethod, "must_change_password": u.PasswordChangeRequired})
	}))
	h.mux.HandleFunc("POST /portal/password", h.withSession(true, h.changePassword))
	h.mux.HandleFunc("POST /portal/logout", h.withSession(true, h.signout))
	h.mux.HandleFunc("GET /portal/api/me", h.authenticated(h.me))
	h.mux.HandleFunc("GET /portal/api/keys", h.authenticated(h.keys))
	h.mux.HandleFunc("POST /portal/api/keys", h.authenticated(h.createKey))
	h.mux.HandleFunc("DELETE /portal/api/keys/{id}", h.authenticated(h.revokeKey))
	h.mux.HandleFunc("GET /portal/api/usage", h.authenticated(h.usage))
	h.mux.HandleFunc("DELETE /portal/api/usage", h.authenticated(h.deleteUsage))
	h.mux.HandleFunc("GET /portal/api/usage/daily", h.authenticated(h.daily))
	h.mux.HandleFunc("GET /portal/api/requests", h.authenticated(h.requests))
	h.mux.HandleFunc("GET /portal/api/requests/{id}", h.authenticated(h.requestTrace))
	h.mux.HandleFunc("GET /portal/api/trace-preferences", h.authenticated(h.tracePreferences))
	h.mux.HandleFunc("PUT /portal/api/trace-preferences", h.authenticated(h.saveTracePreferences))
	h.mux.HandleFunc("DELETE /portal/api/request-traces", h.authenticated(h.clearTraces))
	h.mux.HandleFunc("GET /portal/api/admin/users", h.admin(h.adminUsers))
	h.mux.HandleFunc("POST /portal/api/admin/users", h.admin(h.createLocalUser))
	h.mux.HandleFunc("GET /portal/api/admin/users/{id}", h.admin(h.adminUser))
	h.mux.HandleFunc("PUT /portal/api/admin/users/{id}/groups", h.admin(h.setLocalGroups))
	h.mux.HandleFunc("POST /portal/api/admin/users/{id}/password", h.admin(h.resetLocalPassword))
	h.mux.HandleFunc("DELETE /portal/api/admin/users/{id}/keys/{key}", h.admin(h.revokeUserKey))
	h.mux.HandleFunc("POST /portal/api/admin/users/{id}/keys/{key}/copy", h.admin(h.copyUserKey))
	h.mux.HandleFunc("PUT /portal/api/admin/users/{id}/overrides/{field}", h.admin(h.setOverride))
	h.mux.HandleFunc("DELETE /portal/api/admin/users/{id}/overrides/{field}", h.admin(h.setOverride))
	h.mux.HandleFunc("PUT /portal/api/admin/users/{id}/enabled", h.admin(h.setEnabled))
	h.mux.HandleFunc("GET /portal/api/admin/groups", h.admin(h.adminGroups))
	h.mux.HandleFunc("GET /portal/api/admin/replicas", h.admin(h.adminReplicas))
	h.mux.HandleFunc("GET /portal/api/admin/endpoints", h.admin(h.endpointList))
	h.mux.HandleFunc("PUT /portal/api/admin/endpoints", h.admin(h.endpointSave))
	h.mux.HandleFunc("POST /portal/api/admin/endpoints/test", h.admin(h.endpointTest))
	h.mux.HandleFunc("POST /portal/api/admin/endpoints/models", h.admin(h.endpointModels))
	h.mux.HandleFunc("POST /portal/api/admin/endpoints/restore", h.admin(h.endpointRestore))
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

func (h *Handler) Close() { h.cancel(); <-h.done }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	h.mux.ServeHTTP(w, r.WithContext(ctx))
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
