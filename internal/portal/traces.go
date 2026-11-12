package portal

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/jackc/pgx/v5"
)

func (h *Handler) tracePreferences(w http.ResponseWriter, r *http.Request, u user) {
	if h.Traces == nil {
		unavailable(w)
		return
	}
	p, err := h.Traces.Preferences(r.Context(), u.ID)
	if err != nil {
		unavailable(w)
		return
	}
	p.Available = h.Traces.Enabled()
	respond(w, 200, p)
}
func (h *Handler) saveTracePreferences(w http.ResponseWriter, r *http.Request, u user) {
	if h.Traces == nil {
		unavailable(w)
		return
	}
	var body struct {
		Enabled       *bool `json:"enabled"`
		RetentionDays int   `json:"retention_days"`
	}
	if !h.jsonBody(w, r, &body) {
		return
	}
	if body.Enabled == nil || body.RetentionDays < 1 || body.RetentionDays > 30 {
		fail(w, 400, "invalid_request_error", "enabled is required; retention_days must be between 1 and 30")
		return
	}
	p := requesttrace.Preferences{Enabled: *body.Enabled, RetentionDays: body.RetentionDays}
	if err := h.Traces.Save(r.Context(), u.ID, p); err != nil {
		unavailable(w)
		return
	}
	p.Available = h.Traces.Enabled()
	respond(w, 200, p)
}
func (h *Handler) clearTraces(w http.ResponseWriter, r *http.Request, u user) {
	if h.Traces == nil {
		unavailable(w)
		return
	}
	var body struct{}
	if !h.jsonBody(w, r, &body) {
		return
	}
	_, err := h.Traces.Erase(r.Context(), u.ID, false)
	if err != nil {
		unavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handler) requestTrace(w http.ResponseWriter, r *http.Request, u user) {
	id := r.PathValue("id")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		fail(w, 404, "not_found_error", "trace not found")
		return
	}
	var detail json.RawMessage
	var expires time.Time
	// No admin override: content belongs only to the attributed end user.
	err = h.pool.QueryRow(r.Context(), `SELECT detail,expires_at FROM request_traces WHERE request_id=$1 AND user_id=$2 AND expires_at>now()`, id, u.ID).Scan(&detail, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, 404, "not_found_error", "trace not found")
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"request_id": id, "expires_at": expires, "detail": detail})
}
