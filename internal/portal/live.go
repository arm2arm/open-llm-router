package portal

import (
	"encoding/json"
	"net/http"
	"time"
)

// LiveHeartbeat keeps proxies from closing an idle stream and bounds how long a
// signed-out or expired session can keep receiving events.
var LiveHeartbeat = 15 * time.Second

// requestEvents streams metadata-only events for the session user's requests
// as server-sent events. It never sends content: a trace is fetched as usual.
func (h *Handler) requestEvents(w http.ResponseWriter, r *http.Request, u user) {
	if h.Activity == nil {
		unavailable(w)
		return
	}
	sub, ok := h.Activity.Subscribe(u.ID)
	if !ok {
		fail(w, 429, "rate_limit_error", "too many live connections; close another My requests tab")
		return
	}
	defer h.Activity.Unsubscribe(sub)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(text string) bool {
		if _, err := w.Write([]byte(text)); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 3000\nevent: ready\ndata: {}\n\n") {
		return
	}
	ticker := time.NewTicker(LiveHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.Activity.Done():
			send("event: end\ndata: {\"reason\":\"shutdown\"}\n\n")
			return
		case e := <-sub.C:
			data, _ := json.Marshal(e)
			if !send("data: " + string(data) + "\n\n") {
				return
			}
		case <-ticker.C:
			if _, err := h.session(r); err != nil {
				send("event: end\ndata: {\"reason\":\"session\"}\n\n")
				return
			}
			if !send(": ping\n\n") {
				return
			}
		}
	}
}
