package portal

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// admin wraps a handler for members of a group with admin: true. Membership
// is read from the stored groups on every request, so removing someone from
// the group in the identity provider takes effect at their next login.
func (h *Handler) admin(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return h.authenticated(func(w http.ResponseWriter, r *http.Request, u user) {
		if !h.access(u).Admin {
			fail(w, 403, "permission_error", "admin access is required")
			return
		}
		next(w, r, u)
	})
}

type adminUser struct {
	ID                 string            `json:"id"`
	Issuer             string            `json:"issuer"`
	Username           string            `json:"username"`
	Email              string            `json:"email"`
	Groups             []string          `json:"groups"`
	Enabled            bool              `json:"enabled"`
	SyncedAt           time.Time         `json:"synced_at"`
	ActiveKeys         int64             `json:"active_keys"`
	TokensToday        int64             `json:"tokens_today"`
	TokensMonth        int64             `json:"tokens_this_month"`
	Overrides          map[string]*int64 `json:"overrides"`
	ProtectedAdmin     bool              `json:"protected_admin"`
	HasLocalPassword   bool              `json:"has_local_password"`
	MustChangePassword bool              `json:"must_change_password"`
}

func (h *Handler) calendar() (day, month time.Time) {
	now := h.limits.Now().In(h.cfg.Limits.Location)
	day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return day, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}

// adminUsers lists users (optionally matching part of a username or email)
// with key counts, this day's and month's tokens and overrides.
func (h *Handler) adminUsers(w http.ResponseWriter, r *http.Request, _ user) {
	match := strings.TrimSpace(r.URL.Query().Get("match"))
	day, month := h.calendar()
	rows, err := h.pool.Query(r.Context(), `SELECT u.id, u.issuer, u.username, u.email, u.groups, u.enabled, u.synced_at,u.protected_admin,
		(SELECT count(*) FROM api_keys k WHERE k.user_id = u.id AND NOT k.revoked AND (k.expires IS NULL OR k.expires > clock_timestamp())),
		COALESCE((SELECT sum(d.input_tokens + d.output_tokens) FROM usage_daily d WHERE d.user_id = u.id AND d.day = $2::date), 0)::bigint,
		COALESCE((SELECT sum(d.input_tokens + d.output_tokens) FROM usage_daily d WHERE d.user_id = u.id AND d.day >= $3::date), 0)::bigint,
		(SELECT jsonb_object_agg(o.field, o.value) FROM quota_overrides o WHERE o.user_id = u.id),
		EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=u.id),
		COALESCE((SELECT must_change_password FROM local_credentials c WHERE c.user_id=u.id),false)
		FROM users u
		WHERE $1 = '' OR strpos(lower(u.username), lower($1)) > 0 OR strpos(lower(u.email), lower($1)) > 0
		ORDER BY u.username, u.issuer, u.id LIMIT 500`, match, day.Format("2006-01-02"), month.Format("2006-01-02"))
	if err != nil {
		unavailable(w)
		return
	}
	defer rows.Close()
	users := make([]adminUser, 0)
	for rows.Next() {
		var u adminUser
		if err := rows.Scan(&u.ID, &u.Issuer, &u.Username, &u.Email, &u.Groups, &u.Enabled, &u.SyncedAt, &u.ProtectedAdmin,
			&u.ActiveKeys, &u.TokensToday, &u.TokensMonth, &u.Overrides, &u.HasLocalPassword, &u.MustChangePassword); err != nil {
			unavailable(w)
			return
		}
		if u.Groups == nil {
			u.Groups = []string{}
		}
		if u.Overrides == nil {
			u.Overrides = map[string]*int64{}
		}
		users = append(users, u)
	}
	if rows.Err() != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"users": users, "override_fields": policy.OverrideFields})
}

// adminUser returns one user's effective limits, overrides with their audit
// fields, and key metadata.
func (h *Handler) adminUser(w http.ResponseWriter, r *http.Request, _ user) {
	id, ok := pathUUID(w, r, "id", "user not found")
	if !ok {
		return
	}
	var target user
	var enabled bool
	var issuer string
	var hasPassword bool
	err := h.pool.QueryRow(r.Context(), `SELECT id, username, email, groups, enabled,protected_admin,
		(SELECT jsonb_object_agg(o.field, o.value) FROM quota_overrides o WHERE o.user_id = users.id),issuer,
		EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=users.id),
		COALESCE((SELECT must_change_password FROM local_credentials c WHERE c.user_id=users.id),false)
		FROM users WHERE id = $1`, id).Scan(&target.ID, &target.Username, &target.Email, &target.Groups, &enabled, &target.ProtectedAdmin, &target.Overrides, &issuer, &hasPassword, &target.PasswordChangeRequired)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 404, "not_found_error", "user not found")
		} else {
			unavailable(w)
		}
		return
	}
	admin := auth.Admin{Pool: h.pool}
	overrides, err := admin.ListOverrides(r.Context(), id)
	if err != nil {
		unavailable(w)
		return
	}
	keys, err := admin.ListKeys(r.Context(), id)
	if err != nil {
		unavailable(w)
		return
	}
	if target.Groups == nil {
		target.Groups = []string{}
	}
	access := h.access(target)
	cfg := h.routingConfig()
	models := []string{}
	for name := range cfg.Models {
		if access.AllowsModel(name) {
			models = append(models, name)
		}
	}
	for alias, model := range cfg.Aliases {
		if access.AllowsModel(model) {
			models = append(models, alias)
		}
	}
	slices.Sort(models)
	respond(w, 200, map[string]any{"user": struct {
		user
		Issuer           string `json:"issuer"`
		HasLocalPassword bool   `json:"has_local_password"`
	}{target, issuer, hasPassword}, "enabled": enabled, "limits": limits.Effective(access),
		"models": models, "admin": access.Admin, "overrides": overrides, "keys": keys})
}

// setOverride sets or clears one override field: {"value": N} or
// {"value": null} for unlimited, with an optional reason; DELETE clears it.
func (h *Handler) setOverride(w http.ResponseWriter, r *http.Request, by user) {
	var body struct {
		Value  *int64 `json:"value"`
		Reason string `json:"reason"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	id, ok := pathUUID(w, r, "id", "user not found")
	if !ok {
		return
	}
	field := r.PathValue("field")
	admin := auth.Admin{Pool: h.pool}
	var err error
	if r.Method == http.MethodDelete {
		err = admin.ClearOverride(r.Context(), id, field)
	} else {
		body.Reason = strings.TrimSpace(body.Reason)
		if !policy.ValidOverride(field, body.Value) || len(body.Reason) > 200 {
			fail(w, 400, "invalid_request_error", "unknown field, invalid value, or a reason over 200 bytes")
			return
		}
		err = admin.SetOverride(r.Context(), id, field, body.Value, body.Reason, "portal:"+by.Username)
	}
	if errors.Is(err, auth.ErrNotFound) {
		fail(w, 404, "not_found_error", "user or override not found")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setEnabled disables or re-enables a user: {"enabled": false}. Disabling
// ends their sessions and API access at once.
func (h *Handler) setEnabled(w http.ResponseWriter, r *http.Request, by user) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		fail(w, 400, "invalid_request_error", "enabled must be true or false")
		return
	}
	id, ok := pathUUID(w, r, "id", "user not found")
	if !ok {
		return
	}
	if id == by.ID && !*body.Enabled {
		fail(w, 400, "invalid_request_error", "you cannot disable yourself")
		return
	}
	err := (auth.Admin{Pool: h.pool}).SetEnabled(r.Context(), id, *body.Enabled)
	if errors.Is(err, auth.ErrProtectedAdmin) {
		fail(w, 403, "permission_error", "the protected local administrator cannot be disabled")
		return
	}
	if errors.Is(err, auth.ErrNotFound) {
		fail(w, 404, "not_found_error", "user not found")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type groupTotals struct {
	Group       string `json:"group"`
	Members     int64  `json:"members"`
	Requests    int64  `json:"requests_today"`
	TokensToday int64  `json:"tokens_today"`
	TokensMonth int64  `json:"tokens_this_month"`
	Admin       bool   `json:"admin"`
}

// adminGroups totals usage by configured group. A user in several groups
// counts in each of them.
func (h *Handler) adminGroups(w http.ResponseWriter, r *http.Request, _ user) {
	day, month := h.calendar()
	names := slices.Sorted(maps.Keys(h.cfg.Groups))
	rows, err := h.pool.Query(r.Context(), `SELECT g.name,
		(SELECT count(*) FROM users u WHERE g.name = ANY(u.groups) AND u.enabled),
		COALESCE(sum(d.requests) FILTER (WHERE d.day = $2::date), 0)::bigint,
		COALESCE(sum(d.input_tokens + d.output_tokens) FILTER (WHERE d.day = $2::date), 0)::bigint,
		COALESCE(sum(d.input_tokens + d.output_tokens), 0)::bigint
		FROM unnest($1::text[]) AS g(name)
		LEFT JOIN users u ON g.name = ANY(u.groups)
		LEFT JOIN usage_daily d ON d.user_id = u.id AND d.day >= $3::date
		GROUP BY g.name ORDER BY g.name`, names, day.Format("2006-01-02"), month.Format("2006-01-02"))
	if err != nil {
		unavailable(w)
		return
	}
	defer rows.Close()
	groups := make([]groupTotals, 0, len(names))
	for rows.Next() {
		var g groupTotals
		if err := rows.Scan(&g.Group, &g.Members, &g.Requests, &g.TokensToday, &g.TokensMonth); err != nil {
			unavailable(w)
			return
		}
		g.Admin = h.cfg.Groups[g.Group].Admin
		groups = append(groups, g)
	}
	if rows.Err() != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"groups": groups})
}

type modelReplicas struct {
	Model    string       `json:"model"`
	Replicas []pool.State `json:"replicas"`
}

// adminReplicas shows each pool's replicas: origin, health, in-flight calls
// and capacity. Backend credentials are never part of the state.
func (h *Handler) adminReplicas(w http.ResponseWriter, _ *http.Request, _ user) {
	models := make([]modelReplicas, 0)
	pools := h.Pools
	if h.Endpoints != nil {
		_, pools = h.Endpoints.Runtime.Snapshot()
	}
	if pools != nil {
		for _, name := range slices.Sorted(maps.Keys(pools.Pools)) {
			models = append(models, modelReplicas{Model: name, Replicas: pools.Pools[name].Snapshot()})
		}
	}
	respond(w, 200, map[string]any{"models": models})
}

func pathUUID(w http.ResponseWriter, r *http.Request, name, missing string) (string, bool) {
	var id pgtype.UUID
	if id.Scan(r.PathValue(name)) != nil || !id.Valid {
		fail(w, 404, "not_found_error", missing)
		return "", false
	}
	return r.PathValue(name), true
}
