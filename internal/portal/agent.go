package portal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/reqid"
	"github.com/jackc/pgx/v5"
)

type agentContext struct{}
type agentIdentity struct {
	Key  adminaccess.Key
	User user
	IP   netip.Addr
}
type agentBucket struct {
	Start         time.Time
	Count, Active int
}
type agentLimiter struct {
	mu      sync.Mutex
	buckets map[string]*agentBucket
}

func (l *agentLimiter) acquire(id string, limit, concurrent int) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.buckets == nil {
		l.buckets = map[string]*agentBucket{}
	}
	for k, b := range l.buckets {
		if now.Sub(b.Start) > time.Minute && b.Active == 0 {
			delete(l.buckets, k)
		}
	}
	b := l.buckets[id]
	if b == nil {
		if len(l.buckets) >= 4096 {
			return nil, false
		}
		b = &agentBucket{Start: now}
		l.buckets[id] = b
	}
	if now.Sub(b.Start) >= time.Minute {
		b.Start = now
		b.Count = 0
	}
	if b.Count >= limit || b.Active >= concurrent {
		return nil, false
	}
	b.Count++
	b.Active++
	return func() { l.mu.Lock(); b.Active--; l.mu.Unlock() }, true
}
func positive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
func (h *Handler) agentIdentity(r *http.Request) (agentIdentity, error) {
	var identity agentIdentity
	if len(r.Header.Values("Authorization")) != 1 {
		return identity, adminaccess.ErrDenied
	}
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return identity, adminaccess.ErrDenied
	}
	k, err := adminaccess.Lookup(r.Context(), h.pool, strings.TrimPrefix(value, "Bearer "))
	if err != nil {
		return identity, err
	}
	u := user{}
	err = h.pool.QueryRow(r.Context(), `SELECT id::text,username,email,groups,protected_admin FROM users WHERE id=$1 AND enabled`, k.OwnerID).Scan(&u.ID, &u.Username, &u.Email, &u.Groups, &u.ProtectedAdmin)
	if err != nil {
		return identity, err
	}
	if !h.access(u).Admin {
		_, _ = h.pool.Exec(r.Context(), `UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE owner_id=$1 AND NOT revoked`, u.ID)
		return identity, adminaccess.ErrDenied
	}
	identity = agentIdentity{Key: k, User: u}
	ip, err := adminaccess.ClientIP(r, h.cfg.AdminAPI.TrustedProxyCIDRs)
	if err != nil {
		return identity, errAgentSource
	}
	identity = agentIdentity{Key: k, User: u, IP: ip}
	if !adminaccess.Contains(k.Networks, ip) {
		return identity, errAgentIP
	}
	transport := "rest"
	if strings.HasSuffix(r.URL.Path, "/mcp") {
		transport = "mcp"
	}
	identity.User.AgentActor = &audit.Actor{Type: "admin_api_key", ID: u.ID, KeyID: k.ID, SourceIP: ip.String(), Transport: transport, RequestID: reqid.FromContext(r.Context())}
	return identity, nil
}

var errAgentSource = errors.New("client IP cannot be resolved; check the trusted proxy chain")
var errAgentIP = errors.New("source IP is not allowed by this key")

func (h *Handler) agentAuth(next func(http.ResponseWriter, *http.Request, agentIdentity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !h.cfg.AdminAPI.Enabled {
			fail(w, 404, "not_found_error", "agent administration is disabled")
			return
		}
		if values := r.Header.Values("Origin"); len(values) > 0 && (len(values) != 1 || values[0] != h.cfg.Portal.BaseURL) {
			fail(w, 403, "permission_error", "request origin is forbidden")
			return
		}
		globalRelease, globalOK := h.agentLimits.acquire("global", 600, 8)
		if !globalOK {
			agentThrottle(w)
			return
		}
		defer globalRelease()
		release, ok := h.agentLimits.acquire("ip:"+clientAddress(r), 120, 16)
		if !ok {
			agentThrottle(w)
			return
		}
		defer release()
		identity, err := h.agentIdentity(r)
		if err != nil {
			if errors.Is(err, errAgentIP) {
				h.agentAudit(r, identity, "agent.access", audit.Refused, "")
				fail(w, 403, "ip_not_allowed", err.Error())
			} else if errors.Is(err, errAgentSource) {
				fail(w, 403, "client_ip_unavailable", err.Error())
			} else if errors.Is(err, adminaccess.ErrDenied) || errors.Is(err, pgx.ErrNoRows) {
				fail(w, 401, "authentication_error", "invalid administration credential")
			} else {
				fail(w, 503, "api_error", "administration authentication unavailable")
			}
			return
		}
		releaseKey, ok := h.agentLimits.acquire("key:"+identity.Key.ID, positive(h.cfg.AdminAPI.RequestsPerMinute, 60), positive(h.cfg.AdminAPI.MaxConcurrent, 4))
		if !ok {
			agentThrottle(w)
			return
		}
		defer releaseKey()
		ctx := context.WithValue(r.Context(), agentContext{}, identity)
		ctx = audit.WithActor(ctx, *identity.User.AgentActor)
		_, err = h.pool.Exec(ctx, `UPDATE admin_api_keys SET last_used=clock_timestamp(),last_ip=$2 WHERE id=$1`, identity.Key.ID, identity.IP.String())
		if err != nil {
			unavailable(w)
			return
		}
		next(w, r.WithContext(ctx), identity)
	}
}
func agentThrottle(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	fail(w, 429, "rate_limit_error", "administration request limit reached")
}
func (h *Handler) agentAudit(r *http.Request, a agentIdentity, action, result, change string) {
	actor := audit.Actor{Type: "admin_api_key", ID: a.User.ID, KeyID: a.Key.ID, SourceIP: a.IP.String(), RequestID: reqid.FromContext(r.Context()), ChangeID: change}
	if a.User.AgentActor != nil {
		actor = *a.User.AgentActor
		actor.ChangeID = change
	}
	_ = audit.Record(r.Context(), h.pool, audit.Event{Actor: actor, Action: action, Result: result, TargetType: "admin_change", TargetID: change})
}
func (h *Handler) registerAgent() {
	h.mux.HandleFunc("/api/admin/v1", h.agentAuth(func(w http.ResponseWriter, _ *http.Request, _ agentIdentity) {
		fail(w, 404, "not_found_error", "administration route not found")
	}))
	h.mux.HandleFunc("GET /api/admin/v1/identity", h.agentAuth(h.agentMe))
	h.mux.HandleFunc("GET /api/admin/v1/operations", h.agentAuth(h.agentOperations))
	h.mux.HandleFunc("GET /api/admin/v1/openapi.json", h.agentAuth(h.agentOpenAPI))
	h.mux.HandleFunc("POST /api/admin/v1/read/{operation}", h.agentAuth(h.agentRead))
	h.mux.HandleFunc("POST /api/admin/v1/changes/preview", h.agentAuth(h.agentPreview))
	h.mux.HandleFunc("POST /api/admin/v1/changes/{id}/apply", h.agentAuth(h.agentApply))
	h.mux.HandleFunc("GET /api/admin/v1/changes/{id}", h.agentAuth(h.agentChange))
	h.mux.HandleFunc("POST /api/admin/v1/mcp", h.agentAuth(h.agentMCP))
	h.mux.HandleFunc("GET /api/admin/v1/mcp", h.agentAuth(func(w http.ResponseWriter, _ *http.Request, _ agentIdentity) {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	h.mux.HandleFunc("/api/admin/v1/", h.agentAuth(func(w http.ResponseWriter, _ *http.Request, _ agentIdentity) {
		fail(w, 404, "not_found_error", "administration route not found")
	}))
	h.mux.HandleFunc("GET /portal/api/admin/agent-keys", h.admin(h.agentKeys))
	h.mux.HandleFunc("POST /portal/api/admin/agent-keys", h.admin(h.agentIssue))
	h.mux.HandleFunc("PUT /portal/api/admin/agent-keys/{id}", h.admin(h.agentNarrow))
	h.mux.HandleFunc("DELETE /portal/api/admin/agent-keys/{id}", h.admin(h.agentRevoke))
}
func (h *Handler) agentMe(w http.ResponseWriter, _ *http.Request, a agentIdentity) {
	respond(w, 200, map[string]any{"user_id": a.User.ID, "key_id": a.Key.ID, "permissions": a.Key.Permissions, "source_ip": a.IP.String(), "expires": a.Key.Expires, "allowed_networks": a.Key.Networks})
}
func (h *Handler) agentKeys(w http.ResponseWriter, r *http.Request, u user) {
	keys, err := adminaccess.List(r.Context(), h.pool, u.ID)
	if err != nil {
		unavailable(w)
		return
	}
	ip, ipErr := adminaccess.ClientIP(r, h.cfg.AdminAPI.TrustedProxyCIDRs)
	detected := ""
	if ipErr == nil {
		detected = ip.String()
	}
	respond(w, 200, map[string]any{"enabled": h.cfg.AdminAPI.Enabled, "keys": keys, "permissions": adminaccess.Permissions, "source_ip": detected, "peer_ip": clientAddress(r), "proxy_trusted": adminaccess.Contains(h.cfg.AdminAPI.TrustedProxyCIDRs, netip.MustParseAddr(positiveIP(clientAddress(r)))), "address_error": ipErr != nil, "base_url": h.cfg.Portal.BaseURL, "max_lifetime_days": positive(h.cfg.AdminAPI.MaxLifetimeDays, 90), "default_lifetime_days": positive(h.cfg.AdminAPI.DefaultLifetimeDays, 7)})
}
func positiveIP(ip string) string {
	if _, err := netip.ParseAddr(ip); err != nil {
		return "0.0.0.0"
	}
	return ip
}
func (h *Handler) agentIssue(w http.ResponseWriter, r *http.Request, u user) {
	if !h.cfg.AdminAPI.Enabled {
		fail(w, 403, "feature_disabled", "operator must enable admin_api.enabled")
		return
	}
	var body adminaccess.Create
	if !h.jsonBody(w, r, &body) {
		return
	}
	if time.Since(u.SessionCreated) > recentLocalSignIn || (adminaccess.Sensitive(body.Permissions) && !recentLocal(u)) {
		fail(w, 403, "reauthentication_required", "sign in again; sensitive permissions require a local administrator sign-in within 15 minutes")
		return
	}
	h.issueAgentKey(w, r, u, body, nil)
}
func (h *Handler) issueAgentKey(w http.ResponseWriter, r *http.Request, u user, body adminaccess.Create, parent *adminaccess.Key) {
	if body.Days == 0 {
		body.Days = positive(h.cfg.AdminAPI.DefaultLifetimeDays, 7)
	}
	c, err := adminaccess.Validate(body, positive(h.cfg.AdminAPI.MaxLifetimeDays, 90))
	if err != nil {
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	k, secret, err := adminaccess.Issue(r.Context(), h.pool, u.ID, c, parent, positive(h.cfg.AdminAPI.MaxActiveKeys, 10), portalActor(u))
	if errors.Is(err, adminaccess.ErrDenied) {
		fail(w, 403, "permission_error", err.Error())
		return
	}
	if errors.Is(err, adminaccess.ErrLimit) {
		fail(w, 409, "key_limit_error", err.Error())
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 201, map[string]any{"key": secret, "metadata": k})
}
func (h *Handler) agentRevoke(w http.ResponseWriter, r *http.Request, u user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	id, ok := pathUUID(w, r, "id", "key not found")
	if !ok {
		return
	}
	if err := adminaccess.Revoke(r.Context(), h.pool, u.ID, id, portalActor(u)); err != nil {
		fail(w, 404, "not_found_error", "key not found")
		return
	}
	w.WriteHeader(204)
}

// canonicalArguments binds an exact JSON value without depending on object key order.
func canonicalArguments(raw json.RawMessage) ([]byte, error) {
	var value any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func argumentHash(raw json.RawMessage) ([]byte, error) {
	canonical, err := canonicalArguments(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	return sum[:], nil
}

func (h *Handler) agentNarrow(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Permissions []string  `json:"permissions"`
		Networks    []string  `json:"allowed_networks"`
		Expires     time.Time `json:"expires"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	id, ok := pathUUID(w, r, "id", "key not found")
	if !ok {
		return
	}
	networks, err := adminaccess.Networks(body.Networks)
	if err != nil {
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	if err := adminaccess.Narrow(r.Context(), h.pool, u.ID, id, body.Permissions, networks, body.Expires, portalActor(u)); err != nil {
		fail(w, 403, "permission_error", "key authority may only be reduced")
		return
	}
	w.WriteHeader(204)
}
