package portal

import (
	"errors"
	"net/http"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
)

func (h *Handler) endpointService(w http.ResponseWriter) bool {
	if h.Endpoints == nil {
		unavailable(w)
		return false
	}
	return true
}
func endpointError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var validation *endpoints.ValidationError
	var problems config.Problems
	switch {
	case errors.Is(err, endpoints.ErrConflict), errors.Is(err, endpoints.ErrRoutingManaged):
		fail(w, 409, "conflict_error", err.Error())
	case errors.Is(err, endpoints.ErrModel), errors.Is(err, endpoints.ErrRevision):
		fail(w, 404, "not_found_error", err.Error())
	case errors.As(err, &validation), errors.As(err, &problems):
		fail(w, 400, "invalid_request_error", err.Error())
	default:
		unavailable(w)
	}
	return true
}
func (h *Handler) endpointList(w http.ResponseWriter, r *http.Request, _ user) {
	if !h.endpointService(w) {
		return
	}
	entries, err := h.Endpoints.View(r.Context())
	if endpointError(w, err) {
		return
	}
	respond(w, 200, map[string]any{"models": entries, "credential_references": h.Endpoints.Policy.References(), "allowed_private_networks": h.Endpoints.Policy.Networks()})
}

type endpointBody struct {
	Model         string        `json:"model"`
	Revision      *int64        `json:"revision"`
	Configuration *config.Model `json:"configuration"`
}

func (h *Handler) endpointSave(w http.ResponseWriter, r *http.Request, by user) {
	var body endpointBody
	if !h.jsonBody(w, r, &body) || !h.endpointService(w) {
		return
	}
	if body.Revision == nil || *body.Revision < 0 || body.Configuration == nil {
		fail(w, 400, "invalid_request_error", "revision and configuration are required")
		return
	}
	entries, err := h.Endpoints.Save(r.Context(), body.Model, *body.Revision, *body.Configuration, -1, by.ID)
	if endpointError(w, err) {
		return
	}
	respond(w, 200, map[string]any{"models": entries})
}
func (h *Handler) endpointTest(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct {
		Model         string        `json:"model"`
		Configuration *config.Model `json:"configuration"`
	}
	if !h.jsonBody(w, r, &body) || !h.endpointService(w) {
		return
	}
	if body.Configuration == nil {
		fail(w, 400, "invalid_request_error", "configuration is required")
		return
	}
	states, err := h.Endpoints.Test(r.Context(), body.Model, *body.Configuration)
	if err != nil && len(states) == 0 {
		endpointError(w, err)
		return
	}
	message := "All replicas passed their health check."
	if err != nil {
		message = err.Error()
	}
	respond(w, 200, map[string]any{"ok": err == nil, "message": message, "replicas": states})
}
func (h *Handler) endpointModels(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct {
		Model    string                  `json:"model"`
		Replicas []endpoints.ModelSource `json:"replicas"`
	}
	if !h.jsonBody(w, r, &body) || !h.endpointService(w) {
		return
	}
	result, err := h.Endpoints.Discover(r.Context(), body.Model, body.Replicas)
	if endpointError(w, err) {
		return
	}
	respond(w, 200, result)
}
func (h *Handler) endpointRestore(w http.ResponseWriter, r *http.Request, by user) {
	var body struct {
		Model    string `json:"model"`
		Revision *int64 `json:"revision"`
		Target   *int64 `json:"target_revision"`
	}
	if !h.jsonBody(w, r, &body) || !h.endpointService(w) {
		return
	}
	if body.Revision == nil || *body.Revision < 0 || body.Target == nil || *body.Target < 0 {
		fail(w, 400, "invalid_request_error", "revision and target_revision are required; target 0 restores the file configuration")
		return
	}
	entries, err := h.Endpoints.Save(r.Context(), body.Model, *body.Revision, config.Model{}, *body.Target, by.ID)
	if endpointError(w, err) {
		return
	}
	respond(w, 200, map[string]any{"models": entries})
}

func (h *Handler) routingConfig() *config.Config {
	if h.Endpoints != nil {
		cfg, _ := h.Endpoints.Runtime.Snapshot()
		return cfg
	}
	return h.cfg
}
