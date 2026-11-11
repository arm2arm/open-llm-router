package limits

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresStore struct{ pool *pgxpool.Pool }

func (s postgresStore) load(ctx context.Context, user string, period calendar) (ledger, error) {
	days := make(ledger)
	// The aggregates and timing rows must describe the same committed snapshot.
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT to_char(day, 'YYYY-MM-DD'), model, input_tokens, output_tokens
			FROM usage_daily WHERE user_id=$1 AND day >= $2::date AND day < $3::date`,
			user, period.month.Format("2006-01-02"), period.nextMonth.Format("2006-01-02"))
		if err != nil {
			return err
		}
		for rows.Next() {
			var date, model string
			t := &modelTotals{}
			if err := rows.Scan(&date, &model, &t.input, &t.output); err != nil {
				rows.Close()
				return err
			}
			if days[date] == nil {
				days[date] = make(day)
			}
			days[date][model] = t
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT model, COALESCE(sum(ttfb_ms::double precision), 0), count(ttfb_ms),
			COALESCE(sum(output_tokens::double precision * 1000 / NULLIF(latency_ms - ttfb_ms, 0))
				FILTER (WHERE is_stream AND output_tokens > 0 AND latency_ms > ttfb_ms), 0),
			count(*) FILTER (WHERE is_stream AND output_tokens > 0 AND latency_ms > ttfb_ms)
			FROM usage WHERE (forwarded_for=$1 OR (user_id=$1 AND forwarded_for IS NULL)) AND ts >= $2 AND ts < $3 GROUP BY model`, user, period.day, period.nextDay)
		if err != nil {
			return err
		}
		defer rows.Close()
		date := period.day.Format("2006-01-02")
		if days[date] == nil {
			days[date] = make(day)
		}
		for rows.Next() {
			var model string
			var t modelTotals
			if err := rows.Scan(&model, &t.ttfbSum, &t.ttfbCount, &t.rateSum, &t.rateCount); err != nil {
				return err
			}
			if days[date][model] == nil {
				days[date][model] = &modelTotals{}
			}
			target := days[date][model]
			target.ttfbSum, target.ttfbCount = t.ttfbSum, t.ttfbCount
			target.rateSum, target.rateCount = t.rateSum, t.rateCount
		}
		return rows.Err()
	})
	return days, err
}
