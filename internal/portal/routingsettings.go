package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/idpcheck"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/settings"
)

// RoutingRuntime publishes complete routing configurations: the proxy.
type RoutingRuntime interface {
	Snapshot() (*config.Config, *pool.Manager)
	PrepareRouting(ctx context.Context, cfg *config.Config, models []string, strict bool) (func(), map[string][]pool.State, error)
}

func (h *Handler) routingAdmin(next func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return h.admin(func(w http.ResponseWriter, r *http.Request, u user) {
		if h.Routing == nil || h.RoutingRuntime == nil {
			unavailable(w)
			return
		}
		next(w, r, u)
	})
}

type routingView struct {
	Settings settings.RoutingView `json:"settings"`
	Runtime  struct {
		Revision int64 `json:"revision"` // 0: the file and per-model endpoint revisions
	} `json:"runtime"`
	CredentialReferences   []string `json:"credential_references"`
	AllowedPrivateNetworks []string `json:"allowed_private_networks"`
	// GroupGrants maps each group policy to the models it grants ("*": all).
	// Policies and routing are changed together; references must remain valid.
	GroupGrants             map[string][]string `json:"group_grants"`
	AffectedUsers           []policyImpact      `json:"affected_users"`
	ImpactAvailable         bool                `json:"impact_available"`
	EncryptionKeyConfigured bool                `json:"encryption_key_configured"`
}

func (h *Handler) routingSettings(w http.ResponseWriter, r *http.Request, _ user) {
	view, err := h.Routing.View(r.Context(), settings.DomainRouting)
	if settingsError(w, err) {
		return
	}
	out := routingView{Settings: view, CredentialReferences: []string{}, AllowedPrivateNetworks: []string{}, GroupGrants: map[string][]string{}}
	out.EncryptionKeyConfigured = h.Routing.Cipher != nil
	running, _ := h.RoutingRuntime.Snapshot()
	for name, group := range running.Groups {
		out.GroupGrants[name] = slices.Clone(group.Models)
	}
	if snapshot := h.routingRuntime.Load(); snapshot != nil {
		out.Runtime.Revision = snapshot.Revision
	}
	if h.Endpoints != nil && h.Endpoints.Policy != nil {
		out.CredentialReferences, out.AllowedPrivateNetworks = h.Endpoints.Policy.References(), h.Endpoints.Policy.Networks()
	}
	if view.Draft != nil {
		candidate, _, err := h.Routing.Candidate(r.Context(), settings.DomainRouting, view.Draft.ID)
		var invalid *settings.ValidationError
		if err != nil && !errors.As(err, &invalid) {
			settingsError(w, err)
			return
		}
		if err == nil {
			out.AffectedUsers, err = h.policyImpact(r, candidate)
			if err != nil {
				unavailable(w)
				return
			}
			out.ImpactAvailable = true
		}
	}
	respond(w, 200, out)
}

func (h *Handler) importRouting(w http.ResponseWriter, r *http.Request, u user) {
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	_, err := h.Routing.Import(r.Context(), settings.DomainRouting, portalActor(u))
	if settingsError(w, err) {
		return
	}
	h.routingSettings(w, r, u)
}

func (h *Handler) saveRoutingDraft(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Version  int64             `json:"version"`
		Document json.RawMessage   `json:"document"`
		Secrets  map[string]string `json:"secrets"`
	}
	if !h.jsonBodyLimit(w, r, &body, 256<<10) {
		return
	}
	if body.Version <= 0 || len(body.Document) == 0 {
		fail(w, 400, "invalid_request_error", "version and document are required")
		return
	}
	var document settings.Routing
	if err := json.Unmarshal(body.Document, &document); err != nil {
		fail(w, 400, "invalid_request_error", "invalid routing document")
		return
	}
	secrets := map[string][]byte{}
	defer func() {
		for _, value := range secrets {
			clear(value)
		}
	}()
	for id, value := range body.Secrets {
		backend := document.Backends[id]
		if backend == nil {
			fail(w, 400, "invalid_request_error", "choose an existing backend for each API key")
			return
		}
		secrets[backend.SecretName(id)] = []byte(value)
	}
	_, err := h.Routing.SaveDraft(r.Context(), settings.DomainRouting, body.Version, body.Document, secrets, portalActor(u))
	if settingsError(w, err) {
		return
	}
	h.routingSettings(w, r, u)
}

func (h *Handler) discardRoutingDraft(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Version int64 `json:"version"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	_, err := h.Routing.Discard(r.Context(), settings.DomainRouting, body.Version, portalActor(u))
	if settingsError(w, err) {
		return
	}
	h.routingSettings(w, r, u)
}

// changedModels lists models whose definitions differ from the running ones.
func changedModels(running, next *config.Config) []string {
	encode := func(m config.Model) string { data, _ := json.Marshal(m); return string(data) }
	var models []string
	for _, name := range slices.Sorted(maps.Keys(next.Models)) {
		if old, ok := running.Models[name]; !ok || encode(old) != encode(next.Models[name]) || !slices.Equal(old.Pool, next.Models[name].Pool) {
			models = append(models, name)
		}
	}
	return models
}

// probeResults turns replica health into stored check results. Messages name
// the model and replica position only, never origins or credentials.
func probeResults(states map[string][]pool.State) []idpcheck.Result {
	results := []idpcheck.Result{}
	for _, model := range slices.Sorted(maps.Keys(states)) {
		for i, state := range states[model] {
			result := idpcheck.Result{Name: fmt.Sprintf("models.%s.pool[%d]", model, i), Status: idpcheck.Passed, Message: "the health check passed"}
			if !state.Healthy {
				result.Status, result.Message = idpcheck.Failed, "the health check failed; verify origin, health path and credential reference"
			}
			results = append(results, result)
		}
	}
	return results
}

// checkRouting probes the endpoints a revision changes, without publishing.
func (h *Handler) checkRouting(w http.ResponseWriter, r *http.Request, u user) {
	var body struct {
		Revision int64 `json:"revision"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	cfg, revision, err := h.Routing.Candidate(r.Context(), settings.DomainRouting, body.Revision)
	if settingsError(w, err) {
		return
	}
	// Refused destinations are reported without being contacted.
	results := []idpcheck.Result{}
	if problems := h.verifyRouting(r, cfg); len(problems) > 0 {
		for _, problem := range problems {
			results = append(results, idpcheck.Result{Name: "destination", Status: idpcheck.Failed, Message: problem})
		}
	} else {
		running, _ := h.RoutingRuntime.Snapshot()
		models := changedModels(running, cfg)
		if body.Revision == 0 {
			models = slices.Sorted(maps.Keys(cfg.Models))
		}
		_, states, _ := h.RoutingRuntime.PrepareRouting(r.Context(), cfg, models, false)
		results = probeResults(states)
	}
	if settingsError(w, h.routingChecks(r, revision, results, u)) {
		return
	}
	respond(w, 200, map[string]any{"revision": revision, "results": results})
}

func (h *Handler) routingChecks(r *http.Request, revision int64, results []idpcheck.Result, u user) error {
	return h.Routing.RecordChecks(r.Context(), settings.DomainRouting, revision, results, nil, portalActor(u))
}

// activateRouting applies a draft at once: its configuration is validated,
// the replicas of changed models must pass their health checks, and only then
// are the database and the running proxy switched. Rollback and apply publish
// even while endpoints are down.
func (h *Handler) activateRouting(w http.ResponseWriter, r *http.Request, u user) {
	h.switchRouting(w, r, u, "activate")
}

func (h *Handler) rollbackRouting(w http.ResponseWriter, r *http.Request, u user) {
	h.switchRouting(w, r, u, "rollback")
}

func (h *Handler) applyRouting(w http.ResponseWriter, r *http.Request, u user) {
	h.switchRouting(w, r, u, "apply")
}

func (h *Handler) switchRouting(w http.ResponseWriter, r *http.Request, u user, kind string) {
	var body revisionBody
	if !h.jsonBody(w, r, &body) {
		return
	}
	h.routingMu.Lock()
	defer h.routingMu.Unlock()
	if kind == "apply" {
		view, err := h.Routing.View(r.Context(), settings.DomainRouting)
		if settingsError(w, err) {
			return
		}
		if !view.Managed {
			fail(w, 409, "conflict_error", settings.ErrNotManaged.Error())
			return
		}
		body.Revision = view.Active.ID
	} else if body.Version <= 0 || body.Revision <= 0 {
		fail(w, 400, "invalid_request_error", "version and revision are required")
		return
	}
	cfg, revision, err := h.Routing.Candidate(r.Context(), settings.DomainRouting, body.Revision)
	if err == nil {
		if problems := h.verifyRouting(r, cfg); len(problems) > 0 {
			err = &settings.ValidationError{Problems: problems}
		}
	}
	if err != nil {
		h.auditRefusal(r, u, settings.DomainRouting, kind, body.Revision, err)
		settingsError(w, err)
		return
	}
	running, _ := h.RoutingRuntime.Snapshot()
	publish, states, err := h.RoutingRuntime.PrepareRouting(r.Context(), cfg, changedModels(running, cfg), kind == "activate")
	if err != nil {
		_ = h.routingChecks(r, revision, probeResults(states), u)
		h.auditRefusal(r, u, settings.DomainRouting, kind, revision, err)
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	switch kind {
	case "activate":
		_, err = h.Routing.Activate(r.Context(), settings.DomainRouting, body.Version, body.Revision, portalActor(u), true)
	case "rollback":
		_, err = h.Routing.Rollback(r.Context(), settings.DomainRouting, body.Version, body.Revision, portalActor(u))
	default:
		err = audit.Record(r.Context(), h.pool, audit.Event{Action: "settings.apply", Actor: portalActor(u), TargetType: "settings",
			TargetID: settings.DomainRouting, Revision: revision, Result: audit.Succeeded})
	}
	if settingsError(w, err) {
		return
	}
	publish()
	h.SetRoutingRuntime(settings.Snapshot{Domain: settings.DomainRouting, Revision: revision, Config: cfg})
	h.routingSettings(w, r, u)
}

// verifyRouting applies the outbound destination policy before anything is
// contacted.
func (h *Handler) verifyRouting(r *http.Request, cfg *config.Config) []string {
	if h.Routing.Verify == nil {
		return nil
	}
	return h.Routing.Verify(r.Context(), cfg)
}

// auditRefusal records a change refused before reaching the settings
// service, for example by destination policy or a failed probe.
func (h *Handler) auditRefusal(r *http.Request, u user, domain, kind string, revision int64, err error) {
	_ = audit.Record(r.Context(), h.pool, audit.Event{Action: "settings." + kind, Actor: portalActor(u), TargetType: "settings",
		TargetID: domain, Revision: revision, Result: audit.Refused, Reason: err.Error()})
}

// SetRoutingRuntime records the routing revision this process runs.
func (h *Handler) SetRoutingRuntime(snapshot settings.Snapshot) {
	h.routingRuntime.Store(&snapshot)
	if h.pool == nil || snapshot.Config == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := h.pool.Query(ctx, `SELECT DISTINCT u.id::text,u.groups,u.protected_admin FROM users u JOIN admin_api_keys k ON k.owner_id=u.id WHERE NOT k.revoked`)
	if err != nil {
		return
	}
	var revoked []string
	for rows.Next() {
		var id string
		var groups []string
		var protected bool
		if rows.Scan(&id, &groups, &protected) == nil && !h.access(user{ID: id, Groups: groups, ProtectedAdmin: protected}).Admin {
			revoked = append(revoked, id)
		}
	}
	rows.Close()
	if len(revoked) > 0 {
		_, _ = h.pool.Exec(ctx, `UPDATE admin_api_keys SET revoked=true,policy_version=policy_version+1 WHERE owner_id=ANY($1::uuid[]) AND NOT revoked`, revoked)
	}
}
