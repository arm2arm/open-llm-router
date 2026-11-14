package portal

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Memberships are hypothetical; stored per-user overrides always win. Protected
// administrator authority is derived only from the database, never the request.
func (h *Handler) previewPolicies(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct {
		Revision    int64           `json:"revision"`
		Document    json.RawMessage `json:"document"`
		UserID      string          `json:"user_id"`
		Memberships *[]string       `json:"memberships"`
	}
	if !h.jsonBodyLimit(w, r, &body, 256<<10) {
		return
	}
	if body.Revision < 0 || body.Revision > 0 && len(body.Document) > 0 || body.Memberships != nil && len(*body.Memberships) > 64 {
		fail(w, 400, "invalid_request_error", "choose a document or revision and at most 64 memberships")
		return
	}
	var cfg *config.Config
	var err error
	if len(body.Document) > 0 {
		cfg, err = h.Routing.Preview(r.Context(), body.Document)
	} else {
		cfg, _, err = h.Routing.Candidate(r.Context(), "routing", body.Revision)
	}
	if settingsError(w, err) {
		return
	}
	target := user{}
	if body.UserID != "" {
		var id pgtype.UUID
		if id.Scan(body.UserID) != nil || !id.Valid {
			fail(w, 404, "not_found_error", "user not found")
			return
		}
		err = h.pool.QueryRow(r.Context(), `SELECT id,groups,protected_admin,(SELECT jsonb_object_agg(field,value) FROM quota_overrides WHERE user_id=users.id) FROM users WHERE id=$1`, body.UserID).Scan(&target.ID, &target.Groups, &target.ProtectedAdmin, &target.Overrides)
		if errors.Is(err, pgx.ErrNoRows) {
			fail(w, 404, "not_found_error", "user not found")
			return
		}
		if err != nil {
			unavailable(w)
			return
		}
	}
	if body.Memberships != nil {
		target.Groups = *body.Memberships
	}
	access := policy.ForUser(cfg.Groups, target.Groups, target.Overrides, target.ProtectedAdmin)
	models := []string{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
		if policy.ModelAllowed(cfg, access, name, target.Groups, target.ProtectedAdmin) {
			models = append(models, name)
		}
	}
	aliases := map[string]string{}
	for alias, name := range cfg.Aliases {
		if policy.ModelAllowed(cfg, access, name, target.Groups, target.ProtectedAdmin) {
			aliases[alias] = name
		}
	}
	affected, err := h.policyImpact(r, cfg)
	if err != nil {
		unavailable(w)
		return
	}
	if target.Groups == nil {
		target.Groups = []string{}
	}
	if target.Overrides == nil {
		target.Overrides = map[string]*int64{}
	}
	respond(w, 200, map[string]any{"memberships": target.Groups, "models": models, "aliases": aliases, "admin": access.Admin,
		"priority": access.Priority, "limits": limits.Effective(access), "overrides": target.Overrides, "protected_admin": target.ProtectedAdmin, "affected_users": affected})
}

type policyImpact struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// A bounded page plus an exact count would hide identities from activation
// review. Return every affected identity; no passwords, keys or user content.
func (h *Handler) policyImpact(r *http.Request, next *config.Config) ([]policyImpact, error) {
	current := h.routingConfig()
	rows, err := h.pool.Query(r.Context(), `SELECT id,username,groups,(SELECT jsonb_object_agg(field,value) FROM quota_overrides WHERE user_id=users.id) FROM users WHERE NOT protected_admin ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	affected := []policyImpact{}
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.ID, &u.Username, &u.Groups, &u.Overrides); err != nil {
			return nil, err
		}
		before := policy.ForUser(current.Groups, u.Groups, u.Overrides, false)
		after := policy.ForUser(next.Groups, u.Groups, u.Overrides, false)
		changed := !reflect.DeepEqual(limits.Effective(before), limits.Effective(after)) || before.Admin != after.Admin || before.Priority != after.Priority
		for name := range current.Models {
			allowedBefore := policy.ModelAllowed(current, before, name, u.Groups, false)
			allowedAfter := policy.ModelAllowed(next, after, name, u.Groups, false)
			backendChanged := false
			for _, replica := range current.Models[name].Pool {
				if replica.Backend != "" && !reflect.DeepEqual(current.Backends[replica.Backend], next.Backends[replica.Backend]) {
					backendChanged = true
				}
			}
			changed = changed || allowedBefore != allowedAfter || allowedBefore && (!reflect.DeepEqual(current.Models[name], next.Models[name]) || backendChanged)
		}
		for name := range next.Models {
			_, existed := current.Models[name]
			changed = changed || policy.ModelAllowed(next, after, name, u.Groups, false) && !existed
		}
		for alias, model := range current.Aliases {
			changed = changed || policy.ModelAllowed(current, before, model, u.Groups, false) && next.Aliases[alias] != model
		}
		for alias, model := range next.Aliases {
			changed = changed || policy.ModelAllowed(next, after, model, u.Groups, false) && current.Aliases[alias] != model
		}
		if changed {
			affected = append(affected, policyImpact{u.ID, u.Username})
		}
	}
	return affected, rows.Err()
}
