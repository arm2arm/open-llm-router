package portal

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/modelinfo"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/jackc/pgx/v5"
)

// An integration that signs users in through the router's realm issues each
// user an inference key with the user_keys:issue permission. The permission
// alone reveals no user directory: the caller names a subject it verified
// itself and sees only that user's username, models and limits.

// userModelEntries lists the models and aliases a user may call, as
// GET /v1/models lists them for that user: with context length, type,
// protocols and recipients.
func userModelEntries(cfg *config.Config, pools *pool.Manager, access policy.Policy, groups []string, protectedAdmin bool) []modelinfo.Entry {
	names := slices.AppendSeq(slices.Collect(maps.Keys(cfg.Models)), maps.Keys(cfg.Aliases))
	slices.Sort(names)
	entries := make([]modelinfo.Entry, 0, len(names))
	for _, name := range names {
		target := name
		if canonical, ok := cfg.Aliases[name]; ok {
			target = canonical
		}
		if !policy.ModelAllowed(cfg, access, target, groups, protectedAdmin) {
			continue
		}
		if entry, ok := modelinfo.NewEntry(cfg, pools, name, groups, protectedAdmin); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// issuanceTarget is the stored state that decides whether a user may receive
// an issued key.
type issuanceTarget struct {
	user
	Enabled       bool
	LocalPassword bool
}

func (h *Handler) issuanceTarget(ctx context.Context, issuer, subject string) (issuanceTarget, error) {
	var t issuanceTarget
	err := h.pool.QueryRow(ctx, `SELECT id::text,username,email,groups,enabled,protected_admin,
		(SELECT jsonb_object_agg(o.field,o.value) FROM quota_overrides o WHERE o.user_id=users.id),
		EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=users.id)
		FROM users WHERE issuer=$1 AND sub=$2`, issuer, subject).Scan(&t.ID, &t.Username, &t.Email, &t.Groups, &t.Enabled, &t.ProtectedAdmin, &t.Overrides, &t.LocalPassword)
	if t.Groups == nil {
		t.Groups = []string{}
	}
	return t, err
}

type refusal struct {
	status        int
	kind, message string
}

func (r *refusal) write(w http.ResponseWriter) { fail(w, r.status, r.kind, r.message) }

// issuanceRefusal applies every eligibility rule. It runs on a Keycloak
// identity before the user is recorded, and again on the stored user.
func (h *Handler) issuanceRefusal(t issuanceTarget) *refusal {
	if !t.Enabled {
		return &refusal{403, "user_disabled", "the user is disabled; no key was issued"}
	}
	// Local accounts never qualify; neither does the protected administrator.
	if t.ProtectedAdmin || t.LocalPassword {
		return &refusal{403, "user_not_eligible", "keys cannot be issued for local accounts or the protected administrator"}
	}
	if allowed := h.cfg.AdminAPI.UserKeyIssuance.AllowedGroups; len(allowed) > 0 && !slices.ContainsFunc(t.Groups, func(g string) bool { return slices.Contains(allowed, g) }) {
		return &refusal{403, "user_not_eligible", "the user is in no group allowed by admin_api.user_key_issuance.allowed_groups"}
	}
	if len(userModelEntries(h.routingConfig(), nil, h.access(t.user), t.Groups, t.ProtectedAdmin)) == 0 {
		return &refusal{409, "no_model_access", "the user's policy grants no models; no key was issued"}
	}
	return nil
}

// directoryIdentity reads a user who has not signed in yet from Keycloak.
func (h *Handler) directoryIdentity(ctx context.Context, issuer, subject string) (groupsync.Identity, *refusal) {
	directory, ok := h.GroupSync.(IdentityDirectory)
	if !ok || !h.groupSyncEnabled() {
		return groupsync.Identity{}, &refusal{404, "user_not_found", "the user has not signed in, and the Keycloak group-sync service account is not configured"}
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	identity, err := directory.LookupIdentity(lookup, issuer, subject)
	switch {
	case errors.Is(err, groupsync.ErrUserNotFound), errors.Is(err, groupsync.ErrDisabled):
		return identity, &refusal{404, "user_not_found", "the identity provider has no user with this subject"}
	case err != nil:
		return identity, &refusal{503, "identity_provider_unavailable", "the identity provider could not be read; try again"}
	case !identity.Enabled:
		return identity, &refusal{403, "user_disabled", "the user is disabled in the identity provider; no key was issued"}
	}
	return identity, nil
}

// validSubject accepts an OIDC subject: at most 255 printable ASCII characters.
func validSubject(subject string) bool {
	if subject == "" || len(subject) > 255 || subject == "." || subject == ".." {
		return false
	}
	for _, c := range []byte(subject) {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func (h *Handler) issuedKeyLifetime() (defaults, maximum int) {
	defaults, maximum = h.keyLifetimePolicy()
	if limit := h.cfg.AdminAPI.UserKeyIssuance.MaxLifetimeDays; limit > 0 && limit < maximum {
		maximum = limit
	}
	return min(defaults, maximum), maximum
}

func (h *Handler) issuedKeyLabelPrefix() string {
	if prefix := h.cfg.AdminAPI.UserKeyIssuance.LabelPrefix; prefix != "" {
		return prefix
	}
	return config.DefaultUserKeyLabelPrefix
}

// issueUserKey creates an inference key for an SSO user. Only the configured
// OIDC issuer qualifies. A user who has never signed in is recorded from
// Keycloak first, when the group-sync service account is configured.
func (h *Handler) issueUserKey(w http.ResponseWriter, r *http.Request, _ user) {
	agent, ok := r.Context().Value(agentContext{}).(agentIdentity)
	if !ok {
		fail(w, 403, "permission_error", "an administration key is required")
		return
	}
	var body struct {
		Issuer        string `json:"issuer"`
		Subject       string `json:"subject"`
		Label         string `json:"label"`
		ExpiresInDays *int   `json:"expires_in_days"`
		Class         string `json:"class"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	prefix := h.issuedKeyLabelPrefix()
	label := strings.TrimSpace(body.Label)
	if room := 100 - len(prefix); label == "" || len(label) > room || !utf8.ValidString(label) || strings.ContainsFunc(label, unicode.IsControl) {
		fail(w, 400, "invalid_request_error", fmt.Sprintf("label must contain 1 to %d bytes of printable text after the %q prefix", room, prefix))
		return
	}
	if !validSubject(body.Subject) {
		fail(w, 400, "invalid_request_error", "subject must contain 1 to 255 printable ASCII characters")
		return
	}
	days, maximum := h.issuedKeyLifetime()
	if body.ExpiresInDays != nil {
		days = *body.ExpiresInDays
		if days < 1 || days > maximum {
			fail(w, 400, "invalid_request_error", fmt.Sprintf("expires_in_days must be between 1 and %d days (site maximum)", maximum))
			return
		}
	}
	class := body.Class
	if class == "" {
		class = "interactive"
	}
	if class != "interactive" && class != "batch" {
		fail(w, 400, "invalid_request_error", "class must be interactive or batch")
		return
	}
	issuer := h.current().issuer()
	if issuer == "" || issuer == "local" || body.Issuer != issuer {
		fail(w, 403, "issuer_not_allowed", "keys can be issued only for users of the configured OIDC issuer")
		return
	}
	ctx := r.Context()
	target, err := h.issuanceTarget(ctx, issuer, body.Subject)
	created := false
	if errors.Is(err, pgx.ErrNoRows) {
		identity, refused := h.directoryIdentity(ctx, issuer, body.Subject)
		if refused != nil {
			refused.write(w)
			return
		}
		// Check the candidate before recording anyone the caller cannot serve.
		candidate := issuanceTarget{user: user{Username: identity.Username, Email: identity.Email, Groups: identity.Groups}, Enabled: identity.Enabled}
		if candidate.Groups == nil {
			candidate.Groups = []string{}
		}
		if refused := h.issuanceRefusal(candidate); refused != nil {
			refused.write(w)
			return
		}
		_, err = (auth.Admin{Pool: h.pool}).SyncIdentity(ctx, issuer, body.Subject, identity.Username, identity.Email, identity.Groups, identity.EmailVerified)
		if errors.Is(err, auth.ErrNotFound) {
			fail(w, 403, "user_disabled", "the user is disabled; no key was issued")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
		created = true
		target, err = h.issuanceTarget(ctx, issuer, body.Subject)
	}
	if err != nil {
		unavailable(w)
		return
	}
	if refused := h.issuanceRefusal(target); refused != nil {
		refused.write(w)
		return
	}
	// One routing snapshot gives the access policy and the model list.
	cfg, pools := h.routingSnapshot()
	access := policy.ForUser(cfg.Groups, target.Groups, target.Overrides, target.ProtectedAdmin)
	key, err := (auth.Admin{Pool: h.pool, KeyPolicy: h.cfg.Auth}).CreateIssuedKey(ctx, target.ID, prefix+label, class, time.Duration(days)*24*time.Hour, agent.Key.ID)
	if errors.Is(err, auth.ErrKeyLimit) {
		fail(w, http.StatusConflict, "key_limit_error", err.Error())
		return
	}
	if errors.Is(err, auth.ErrKeyLifetime) {
		fail(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 201, map[string]any{
		"key": key.Secret, "id": key.ID, "prefix": key.Prefix, "label": key.Label, "class": key.Class, "expires": key.Expires,
		"base_url": h.cfg.Portal.BaseURL + "/v1",
		"user":     map[string]any{"username": target.Username, "created": created},
		"models":   userModelEntries(cfg, pools, access, target.Groups, target.ProtectedAdmin),
		"limits":   limits.Effective(access),
	})
}

// revokeIssuedKey revokes a key the calling administration key issued.
// Every other key is reported as not found.
func (h *Handler) revokeIssuedKey(w http.ResponseWriter, r *http.Request, _ user) {
	agent, ok := r.Context().Value(agentContext{}).(agentIdentity)
	if !ok {
		fail(w, 403, "permission_error", "an administration key is required")
		return
	}
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	keyID, ok := pathUUID(w, r, "key_id", "key not found")
	if !ok {
		return
	}
	err := (auth.Admin{Pool: h.pool}).RevokeIssuedKey(r.Context(), agent.Key.ID, keyID)
	if errors.Is(err, auth.ErrNotFound) {
		fail(w, 404, "not_found_error", "key not found")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// issuedKeySummary reports a key the calling administration key issued:
// expiry, revocation, last use, models and today's usage against the
// owner's limits. Quotas are per user, so usage covers all the user's keys.
func (h *Handler) issuedKeySummary(w http.ResponseWriter, r *http.Request, _ user) {
	agent, ok := r.Context().Value(agentContext{}).(agentIdentity)
	if !ok {
		fail(w, 403, "permission_error", "an administration key is required")
		return
	}
	keyID, ok := pathUUID(w, r, "key_id", "key not found")
	if !ok {
		return
	}
	var key auth.KeyInfo
	var target user
	var enabled bool
	err := h.pool.QueryRow(r.Context(), `SELECT k.id::text,k.prefix,k.label,k.class,k.created,k.last_used,k.expires,k.revoked,COALESCE(k.expires<=clock_timestamp(),false),
		u.id::text,u.username,u.groups,u.enabled,u.protected_admin,(SELECT jsonb_object_agg(o.field,o.value) FROM quota_overrides o WHERE o.user_id=u.id)
		FROM api_keys k JOIN users u ON u.id=k.user_id WHERE k.id=$1 AND k.issued_by_admin_key=$2`, keyID, agent.Key.ID).
		Scan(&key.ID, &key.Prefix, &key.Label, &key.Class, &key.Created, &key.LastUsed, &key.Expires, &key.Revoked, &key.Expired, &target.ID, &target.Username, &target.Groups, &enabled, &target.ProtectedAdmin, &target.Overrides)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "not_found_error", "key not found")
		return
	}
	if err != nil || h.limits == nil {
		unavailable(w)
		return
	}
	if target.Groups == nil {
		target.Groups = []string{}
	}
	cfg, pools := h.routingSnapshot()
	access := policy.ForUser(cfg.Groups, target.Groups, target.Overrides, target.ProtectedAdmin)
	quota, err := h.limits.Snapshot(r.Context(), target.ID, access)
	if err != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{
		"id": key.ID, "prefix": key.Prefix, "label": key.Label, "class": key.Class, "created": key.Created,
		"expires": key.Expires, "expired": key.Expired, "revoked": key.Revoked, "last_used": key.LastUsed,
		"active": enabled && !key.Revoked && !key.Expired,
		"user":   map[string]any{"username": target.Username, "enabled": enabled},
		"models": userModelEntries(cfg, pools, access, target.Groups, target.ProtectedAdmin),
		"limits": quota.Limits,
		// Totals only: no per-model breakdown, prompts or request records.
		"usage": map[string]any{"timezone": quota.Timezone, "used": quota.Used, "remaining": quota.Remaining, "reset_at": quota.ResetAt},
	})
}
