package portal

import (
	"net/http"
	"time"

	"github.com/arm2arm/open-llm-router/internal/limits"
)

type modelUsage struct {
	Model        string   `json:"model"`
	Requests     int64    `json:"requests"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	TTFB         *float64 `json:"average_ttfb_ms"`
	TPS          *float64 `json:"average_tokens_per_second"`
}

func (h *Handler) usage(w http.ResponseWriter, r *http.Request, u user) {
	quota, err := h.limits.Snapshot(r.Context(), u.ID, h.access(u))
	if err != nil {
		limits.WriteError(w, "openai", err)
		return
	}
	now := h.limits.Now().In(h.cfg.Limits.Location)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	// Both token aggregates and measurements use one SQL snapshot, and the
	// same local calendar as the quota manager and accounting writer.
	rows, err := h.pool.Query(r.Context(), `WITH windows(name,start_at,end_at) AS (
		VALUES ('today',$2::timestamptz,$3::timestamptz),('month',$4::timestamptz,$5::timestamptz)
	), totals AS (
		SELECT w.name,d.model,sum(d.requests)::bigint AS requests,sum(d.input_tokens)::bigint AS input_tokens,sum(d.output_tokens)::bigint AS output_tokens
		FROM windows w JOIN usage_daily d ON d.user_id=$1 AND d.day>=(w.start_at AT TIME ZONE $6)::date AND d.day<(w.end_at AT TIME ZONE $6)::date
		GROUP BY w.name,d.model
	), timing AS (
		SELECT w.name,a.model,avg(a.ttfb_ms)::double precision AS ttfb,
			avg(a.output_tokens::double precision*1000/NULLIF(a.latency_ms-a.ttfb_ms,0)) FILTER(WHERE a.is_stream AND a.output_tokens>0 AND a.latency_ms>a.ttfb_ms) AS tps
		FROM windows w JOIN usage a ON (a.forwarded_for=$1 OR (a.user_id=$1 AND a.forwarded_for IS NULL)) AND a.ts>=w.start_at AND a.ts<w.end_at GROUP BY w.name,a.model
	) SELECT d.name,d.model,d.requests,d.input_tokens,d.output_tokens,t.ttfb,t.tps
		FROM totals d LEFT JOIN timing t ON t.name=d.name AND t.model=d.model ORDER BY d.name,d.model`, u.ID, day, day.AddDate(0, 0, 1), month, month.AddDate(0, 1, 0), h.cfg.Limits.Location.String())
	if err != nil {
		unavailable(w)
		return
	}
	defer rows.Close()
	today, thisMonth := make([]modelUsage, 0), make([]modelUsage, 0)
	for rows.Next() {
		var window string
		var model modelUsage
		if err := rows.Scan(&window, &model.Model, &model.Requests, &model.InputTokens, &model.OutputTokens, &model.TTFB, &model.TPS); err != nil {
			unavailable(w)
			return
		}
		if window == "today" {
			today = append(today, model)
		} else {
			thisMonth = append(thisMonth, model)
		}
	}
	if rows.Err() != nil {
		unavailable(w)
		return
	}
	respond(w, 200, map[string]any{"quota": quota, "today": today, "month": thisMonth,
		"retention": map[string]int{"usage_days": h.cfg.Retention.UsageDays, "usage_daily_days": h.cfg.Retention.UsageDailyDays}})
}
