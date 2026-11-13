package portal

import (
	"net/http"
	"strconv"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
)

func (h *Handler) adminHistory(w http.ResponseWriter, r *http.Request, _ user) {
	q := r.URL.Query()
	filter := audit.Filter{Action: q.Get("action"), ActorID: q.Get("actor"), TargetID: q.Get("target"), Result: q.Get("result")}
	invalid := func() {
		fail(w, 400, "invalid_request_error", "invalid history filter: use RFC3339 dates, a positive page size up to 200 and a nonnegative cursor")
	}
	var err error
	if v := q.Get("before"); v != "" {
		filter.Before, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			invalid()
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		filter.Limit, err = strconv.Atoi(v)
		if err != nil {
			invalid()
			return
		}
	}
	for name, target := range map[string]**time.Time{"from": &filter.From, "to": &filter.To} {
		if v := q.Get(name); v != "" {
			parsed, err := time.Parse(time.RFC3339, v)
			if err != nil {
				invalid()
				return
			}
			*target = &parsed
		}
	}
	page, err := audit.List(r.Context(), h.pool, filter)
	if err == audit.ErrFilter {
		invalid()
		return
	}
	if err != nil {
		unavailable(w)
		return
	}
	respond(w, 200, page)
}
