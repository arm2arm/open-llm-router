// Package retention deletes usage records older than the configured periods.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// batchSize bounds each delete transaction, so a large backlog never holds
// locks or a long transaction open.
const batchSize = 10000

// lock is held per batch; with several routers only one deletes at a time.
const lock int64 = 0x4f4c5252455445 // "OLRRETE"

// Result counts the rows deleted from each table.
type Result struct {
	Usage      int64 `json:"usage"`
	UsageDaily int64 `json:"usage_daily"`
}

// Cutoffs returns the oldest per-request time and the oldest day kept.
func Cutoffs(cfg config.Retention, location *time.Location, now time.Time) (time.Time, time.Time) {
	local := now.In(location)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	return now.AddDate(0, 0, -cfg.UsageDays), today.AddDate(0, 0, -cfg.UsageDailyDays)
}

// Prune deletes per-request records older than usage_days and daily totals
// older than usage_daily_days, in batches.
func Prune(ctx context.Context, pool *pgxpool.Pool, cfg config.Retention, location *time.Location, now time.Time) (Result, error) {
	usageBefore, dayBefore := Cutoffs(cfg, location, now)
	var result Result
	for _, step := range []struct {
		sql   string
		arg   any
		count *int64
	}{
		{`DELETE FROM usage WHERE ctid IN (SELECT ctid FROM usage WHERE ts < $1 LIMIT $2)`, usageBefore, &result.Usage},
		{`DELETE FROM usage_daily WHERE ctid IN (SELECT ctid FROM usage_daily WHERE day < $1::date LIMIT $2)`, dayBefore.Format("2006-01-02"), &result.UsageDaily},
	} {
		for {
			var deleted int64
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var locked bool
				if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, lock).Scan(&locked); err != nil {
					return err
				}
				if !locked {
					return errBusy
				}
				tag, err := tx.Exec(ctx, step.sql, step.arg, batchSize)
				deleted = tag.RowsAffected()
				return err
			})
			if errors.Is(err, errBusy) {
				return result, nil // another router is pruning
			}
			if err != nil {
				return result, errors.New("delete expired usage records failed")
			}
			*step.count += deleted
			if deleted < batchSize {
				break
			}
		}
	}
	return result, nil
}

var errBusy = errors.New("retention job is running elsewhere")

// NextRun is the next time of day runAt ("HH:MM") in location after now.
func NextRun(runAt string, location *time.Location, now time.Time) time.Time {
	at, _ := time.Parse("15:04", runAt)
	local := now.In(location)
	next := time.Date(local.Year(), local.Month(), local.Day(), at.Hour(), at.Minute(), 0, 0, location)
	if !next.After(local) {
		next = time.Date(local.Year(), local.Month(), local.Day()+1, at.Hour(), at.Minute(), 0, 0, location)
	}
	return next
}

// Run prunes once at start and then daily at cfg.RunAt until ctx ends.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg config.Retention, location *time.Location, log *slog.Logger) {
	for {
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		result, err := Prune(runCtx, pool, cfg, location, time.Now())
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Error("retention job failed", "usage", result.Usage, "usage_daily", result.UsageDaily)
		} else if err == nil {
			log.Info("retention", "usage", result.Usage, "usage_daily", result.UsageDaily,
				"usage_days", cfg.UsageDays, "usage_daily_days", cfg.UsageDailyDays)
		}
		timer := time.NewTimer(time.Until(NextRun(cfg.RunAt, location, time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
