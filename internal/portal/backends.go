package portal

import (
	"net/http"

	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/settings"
)

func (h *Handler) detectBackend(w http.ResponseWriter, r *http.Request, _ user) {
	var body struct {
		ID      string            `json:"id"`
		Backend *settings.Backend `json:"backend"`
		APIKey  *string           `json:"api_key"`
	}
	if !h.jsonBodyLimit(w, r, &body, 16<<10) || !h.endpointService(w) {
		return
	}
	if body.Backend == nil {
		fail(w, 400, "invalid_request_error", "backend is required")
		return
	}
	origin, err := settings.NormalizeBackendURL(body.Backend.URL)
	if err != nil {
		fail(w, 400, "invalid_request_error", err.Error())
		return
	}
	body.Backend.URL = origin
	if body.Backend.HealthPath == "" {
		body.Backend.HealthPath = "/v1/models"
	}
	// An explicit replacement key takes precedence over a stored reference.
	// Backend identity and URL are validated before any probe.
	ref := body.Backend.APIKey
	if body.APIKey != nil {
		body.Backend.APIKey = settings.SecretRef{}
	}
	token, err := settings.BackendToken(r.Context(), h.Routing, body.ID, body.Backend)
	body.Backend.APIKey = ref
	if settingsError(w, err) {
		return
	}
	if body.APIKey != nil {
		if *body.APIKey != "" && !settings.ValidBackendKey(*body.APIKey) {
			fail(w, 400, "invalid_request_error", "API key must be at most 4096 bytes without control characters")
			return
		}
		token = *body.APIKey
	}
	result, err := h.Endpoints.Detect(r.Context(), endpoints.ModelSource{URL: origin, PrivateNetwork: body.Backend.PrivateNetwork}, token)
	if endpointError(w, err) {
		return
	}
	respond(w, 200, result)
}
