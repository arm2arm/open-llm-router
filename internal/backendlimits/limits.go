// Package backendlimits enforces shared backend budgets in PostgreSQL.
package backendlimits

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Manager struct{ DB *pgxpool.Pool }
type Lease struct {
	manager     *Manager
	id, backend string
	done        chan struct{}
	once        sync.Once
}

// Acquire uses a fixed UTC minute for RPM and reported token usage, reserving
// estimated tokens for active requests. Existing streams are never truncated.
func (m *Manager) Acquire(ctx context.Context, backend string, rate config.BackendRate, estimate int64) (*Lease, error) {
	if !rate.Limited() {
		return nil, nil
	}
	if m == nil || m.DB == nil {
		return nil, limits.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	l := &Lease{manager: m, id: rand.Text(), backend: backend, done: make(chan struct{})}
	err := pgx.BeginFunc(ctx, m.DB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO backend_rate_state(backend,window_start) VALUES($1,date_trunc('minute',clock_timestamp())) ON CONFLICT DO NOTHING`, backend)
		if err != nil {
			return err
		}
		var window, now time.Time
		var requests, tokens int64
		if err = tx.QueryRow(ctx, `SELECT window_start,requests,tokens,clock_timestamp() FROM backend_rate_state WHERE backend=$1 FOR UPDATE`, backend).Scan(&window, &requests, &tokens, &now); err != nil {
			return err
		}
		minute := now.UTC().Truncate(time.Minute)
		if window.Before(minute) {
			window, requests, tokens = minute, 0, 0
		}
		if _, err = tx.Exec(ctx, `DELETE FROM backend_rate_leases WHERE backend=$1 AND expires_at<=$2`, backend, now); err != nil {
			return err
		}
		var active, reserved int64
		if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(reserved_tokens),0)::bigint FROM backend_rate_leases WHERE backend=$1`, backend).Scan(&active, &reserved); err != nil {
			return err
		}
		reject := func(name string, reset time.Time) error {
			return &limits.Rejection{Limit: "backend_" + name, ResetAt: reset, RetryAfter: max(1, int(math.Ceil(reset.Sub(now).Seconds())))}
		}
		if rate.MaxConcurrent != nil && active >= int64(*rate.MaxConcurrent) {
			return reject("concurrency", now.Add(time.Second))
		}
		if rate.RPM != nil && requests >= *rate.RPM {
			return reject("rpm", minute.Add(time.Minute))
		}
		reservation := int64(0)
		if rate.TPM != nil {
			reservation = max(1, estimate)
			if tokens >= *rate.TPM || reserved > *rate.TPM-tokens || reservation > *rate.TPM-tokens-reserved {
				return reject("tpm", minute.Add(time.Minute))
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE backend_rate_state SET window_start=$2,requests=$3,tokens=$4 WHERE backend=$1`, backend, window, requests+1, tokens); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO backend_rate_leases(id,backend,reserved_tokens,expires_at) VALUES($1,$2,$3,$4)`, l.id, backend, reservation, now.Add(time.Minute))
		return err
	})
	if err != nil {
		var rejected *limits.Rejection
		if errors.As(err, &rejected) {
			return nil, rejected
		}
		return nil, limits.ErrUnavailable
	}
	go l.heartbeat()
	return l, nil
}

func (l *Lease) heartbeat() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = l.manager.DB.Exec(ctx, `UPDATE backend_rate_leases SET expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, l.id)
			cancel()
		}
	}
}

// Complete releases capacity even after caller cancellation. A dead process's
// leases expire, so a crashed router cannot permanently occupy backend slots.
func (l *Lease) Complete(tokens int64) error {
	if l == nil {
		return nil
	}
	var result error
	l.once.Do(func() {
		close(l.done)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result = pgx.BeginFunc(ctx, l.manager.DB, func(tx pgx.Tx) error {
			var window, now time.Time
			if err := tx.QueryRow(ctx, `SELECT window_start,clock_timestamp() FROM backend_rate_state WHERE backend=$1 FOR UPDATE`, l.backend).Scan(&window, &now); err != nil {
				return err
			}
			var id string
			if err := tx.QueryRow(ctx, `DELETE FROM backend_rate_leases WHERE id=$1 RETURNING id`, l.id).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
				return nil
			} else if err != nil {
				return err
			}
			minute := now.UTC().Truncate(time.Minute)
			_, err := tx.Exec(ctx, `UPDATE backend_rate_state SET tokens=LEAST(1000000000000000,CASE WHEN window_start=$2 THEN tokens ELSE 0 END+$3),requests=CASE WHEN window_start=$2 THEN requests ELSE 0 END,window_start=$2 WHERE backend=$1`, l.backend, minute, min(max(0, tokens), int64(1000000000000000)))
			return err
		})
	})
	return result
}
