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
	"github.com/jackc/pgx/v5/pgtype"
)

func (h *Handler) access(u user) policy.Policy { return policy.Resolve(h.cfg.Groups, u.Groups) }
func effective(p policy.Policy) limits.EffectiveLimits {
	return limits.EffectiveLimits{TokensPerDay: p.TokensPerDay, TokensPerMonth: p.TokensPerMonth, RPM: p.RPM, MaxConcurrent: p.MaxConcurrent}
}
func (h *Handler) me(w http.ResponseWriter, r *http.Request, u user) {
	respond(w, 200, map[string]any{"user": u, "limits": effective(h.access(u))})
}

func (h *Handler) keys(w http.ResponseWriter, r *http.Request, u user) {
	keys, err := (&auth.Admin{Pool: h.pool}).ListKeys(r.Context(), u.ID)
	if err != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"keys": keys})
}
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Label string `json:"label"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	body.Label = strings.TrimSpace(body.Label)
	if body.Label == "" || len(body.Label) > 100 {
		fail(w, 400, "invalid_request_error", "label must contain 1 to 100 bytes")
		return
	}
	key, err := (&auth.Admin{Pool: h.pool}).CreateKey(r.Context(), u.ID, body.Label, time.Duration(h.cfg.Auth.DefaultKeyLifetimeDays)*24*time.Hour)
	if err != nil {
		unavailable(w)
		return
	}
	respond(w, 201, map[string]string{"key": key})
}
func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request, u user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	var id pgtype.UUID
	if id.Scan(r.PathValue("id")) != nil || !id.Valid {
		fail(w, 404, "not_found_error", "key not found")
		return
	}
	err := (&auth.Admin{Pool: h.pool}).RevokeOwnedKey(r.Context(), u.ID, r.PathValue("id"))
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

type model struct {
	ID        string   `json:"id"`
	Canonical string   `json:"canonical_model"`
	Protocols []string `json:"protocols"`
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request, u user) {
	access := h.access(u)
	names := slices.AppendSeq(slices.Collect(maps.Keys(h.cfg.Models)), maps.Keys(h.cfg.Aliases))
	slices.Sort(names)
	models := make([]model, 0, len(names))
	for _, name := range names {
		target := name
		if canonical, ok := h.cfg.Aliases[name]; ok {
			target = canonical
		}
		if access.AllowsModel(target) {
			models = append(models, model{ID: name, Canonical: target, Protocols: h.cfg.Models[target].Protocols})
		}
	}
	respond(w, 200, map[string]any{"router_url": h.cfg.Portal.BaseURL, "base_url": h.cfg.Portal.BaseURL + "/v1", "models": models})
}
