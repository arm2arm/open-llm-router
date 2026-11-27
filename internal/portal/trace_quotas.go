package portal

import (
	"errors"
	"net/http"

	"github.com/arm2arm/open-llm-router/internal/requesttrace"
)

func (h *Handler) traceQuotas(w http.ResponseWriter, r *http.Request, by user) {
	if h.Traces == nil {
		unavailable(w)
		return
	}
	id := ""
	if r.PathValue("id") != "" {
		var ok bool
		id, ok = pathUUID(w, r, "id", "user not found")
		if !ok {
			return
		}
	}
	if r.Method == http.MethodPut {
		var body struct {
			Version   *int64 `json:"version"`
			MaxBytes  *int64 `json:"max_bytes"`
			MaxTraces *int64 `json:"max_traces"`
		}
		if !h.jsonBody(w, r, &body) {
			return
		}
		if body.Version == nil {
			fail(w, 400, "invalid_request_error", "version is required")
			return
		}
		if err := h.Traces.SaveQuotas(r.Context(), id, *body.Version, requesttrace.QuotaOverride{MaxBytes: body.MaxBytes, MaxTraces: body.MaxTraces}, portalActor(by)); traceQuotaError(w, err) {
			return
		}
	}
	v, err := h.Traces.Quotas(r.Context(), id)
	if traceQuotaError(w, err) {
		return
	}
	respond(w, 200, v)
}

func traceQuotaError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, requesttrace.ErrQuota):
		fail(w, 400, "invalid_request_error", err.Error())
	case errors.Is(err, requesttrace.ErrQuotaConflict):
		fail(w, 409, "conflict_error", err.Error())
	case errors.Is(err, requesttrace.ErrQuotaUser):
		fail(w, 404, "not_found_error", err.Error())
	default:
		unavailable(w)
	}
	return true
}
