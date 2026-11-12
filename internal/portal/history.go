package portal

import (
	"net/http"
	"time"
)

// requestLimit is how many recent requests the portal lists.
const requestLimit = 200

type requestRecord struct {
	Operation      string    `json:"operation"`
	RequestID      *string   `json:"request_id"`
	TraceAvailable bool      `json:"trace_available"`
	Time           time.Time `json:"time"`
	Model          string    `json:"model"`
	Protocol       string    `json:"protocol"`
	Status         string    `json:"status"`
	InputTokens    int64     `json:"input_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	LatencyMS      int64     `json:"latency_ms"`
	TTFBMS         *int64    `json:"ttfb_ms"`
	Stream         bool      `json:"stream"`
	KeyPrefix      *string   `json:"key_prefix"` // null once the key is removed
	Forwarded      bool      `json:"forwarded"`  // made by a trusted forwarder for this user
}

// requests lists the newest per-request records attributed to the user:
// metadata only, as stored.
func (h *Handler) requests(w http.ResponseWriter, r *http.Request, u user) {
	rows, err := h.pool.Query(r.Context(), `SELECT a.ts, a.model, a.protocol, a.status, a.input_tokens, a.output_tokens,
		a.latency_ms, a.ttfb_ms, a.is_stream, k.prefix, a.forwarded_for IS NOT NULL, a.request_id, a.operation,
		EXISTS(SELECT 1 FROM request_traces t WHERE t.request_id=a.request_id AND t.user_id=$1 AND t.expires_at>now())
		FROM usage a LEFT JOIN api_keys k ON k.id = a.key_id
		WHERE a.forwarded_for = $1 OR (a.user_id = $1 AND a.forwarded_for IS NULL)
		ORDER BY a.ts DESC LIMIT $2`, u.ID, requestLimit)
	if err != nil {
		unavailable(w)
		return
	}
	defer rows.Close()
	list := make([]requestRecord, 0)
	for rows.Next() {
		var rec requestRecord
		if err := rows.Scan(&rec.Time, &rec.Model, &rec.Protocol, &rec.Status, &rec.InputTokens, &rec.OutputTokens,
			&rec.LatencyMS, &rec.TTFBMS, &rec.Stream, &rec.KeyPrefix, &rec.Forwarded, &rec.RequestID, &rec.Operation, &rec.TraceAvailable); err != nil {
			unavailable(w)
			return
		}
		if rec.Forwarded {
			rec.KeyPrefix = nil // the forwarder's key is not the user's
		}
		list = append(list, rec)
	}
	if rows.Err() != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"requests": list, "limit": requestLimit})
}

type dailyTotal struct {
	Day          string `json:"day"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// daily returns the last 30 days of daily totals, oldest first, with a zero
// row for each day without usage, in limits.timezone.
func (h *Handler) daily(w http.ResponseWriter, r *http.Request, u user) {
	now := h.limits.Now().In(h.cfg.Limits.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	first := today.AddDate(0, 0, -29)
	rows, err := h.pool.Query(r.Context(), `SELECT to_char(g.day, 'YYYY-MM-DD'), COALESCE(sum(d.requests), 0)::bigint,
		COALESCE(sum(d.input_tokens), 0)::bigint, COALESCE(sum(d.output_tokens), 0)::bigint
		FROM generate_series($2::date, $3::date, interval '1 day') AS g(day)
		LEFT JOIN usage_daily d ON d.day = g.day::date AND d.user_id = $1
		GROUP BY g.day ORDER BY g.day`, u.ID, first.Format("2006-01-02"), today.Format("2006-01-02"))
	if err != nil {
		unavailable(w)
		return
	}
	defer rows.Close()
	days := make([]dailyTotal, 0, 30)
	for rows.Next() {
		var d dailyTotal
		if err := rows.Scan(&d.Day, &d.Requests, &d.InputTokens, &d.OutputTokens); err != nil {
			unavailable(w)
			return
		}
		days = append(days, d)
	}
	if rows.Err() != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"timezone": h.cfg.Limits.Location.String(), "days": days})
}
