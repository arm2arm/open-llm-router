package requesttrace

import (
	"context"
	"errors"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/jackc/pgx/v5"
)

var (
	ErrQuota         = errors.New("trace quota requires whole MiB from 1 to 1048576 and a trace count from 1 to 1000000; null inherits the default")
	ErrQuotaConflict = errors.New("trace quotas changed; reload the saved quotas before editing again")
	ErrQuotaUser     = errors.New("user not found")
)

type Quota struct {
	MaxBytes  int64 `json:"max_bytes"`
	MaxTraces int64 `json:"max_traces"`
}

// A nil field inherits the next level: user -> global -> deployment.
type QuotaOverride struct {
	MaxBytes  *int64 `json:"max_bytes"`
	MaxTraces *int64 `json:"max_traces"`
}

type QuotaBounds struct {
	MaxMiB    int64 `json:"max_mib"`
	MaxTraces int64 `json:"max_traces"`
}

type QuotaView struct {
	Version   int64         `json:"version"`
	UpdatedAt *time.Time    `json:"updated_at"`
	Overrides QuotaOverride `json:"overrides"`
	Defaults  Quota         `json:"defaults"`
	Effective Quota         `json:"effective"`
	Storage   *Storage      `json:"storage,omitempty"`
	Bounds    QuotaBounds   `json:"bounds"`
}

func validateQuota(q QuotaOverride) error {
	if q.MaxBytes != nil && (*q.MaxBytes < 1<<20 || *q.MaxBytes > config.MaxTraceStoredMiB<<20 || *q.MaxBytes%(1<<20) != 0) ||
		q.MaxTraces != nil && (*q.MaxTraces < 1 || *q.MaxTraces > config.MaxTraceStoredTraces) {
		return ErrQuota
	}
	return nil
}

// Quotas returns metadata only. An empty user selects the global defaults;
// a user ID selects that account's overrides, effective quota and storage use.
func (s *Service) Quotas(ctx context.Context, user string) (QuotaView, error) {
	v := QuotaView{Defaults: s.quotaDefaults, Bounds: QuotaBounds{MaxMiB: config.MaxTraceStoredMiB, MaxTraces: config.MaxTraceStoredTraces}}
	var err error
	if user == "" {
		err = s.pool.QueryRow(ctx, `SELECT max_bytes,max_traces,version,updated_at FROM trace_quota_defaults WHERE singleton`).Scan(
			&v.Overrides.MaxBytes, &v.Overrides.MaxTraces, &v.Version, &v.UpdatedAt)
	} else {
		v.Storage = &Storage{}
		err = s.pool.QueryRow(ctx, `SELECT o.max_bytes,o.max_traces,COALESCE(o.version,0),o.updated_at,
			COALESCE(d.max_bytes,$2),COALESCE(d.max_traces,$3),t.traces,t.bytes,t.next_expiry
			FROM users u CROSS JOIN trace_quota_defaults d LEFT JOIN trace_quota_overrides o ON o.user_id=u.id
			CROSS JOIN LATERAL (SELECT count(*) AS traces,COALESCE(sum(stored_bytes),0)::bigint AS bytes,min(expires_at) AS next_expiry
				FROM request_traces WHERE user_id=u.id) t
			WHERE u.id=$1 AND d.singleton`, user, s.quotaDefaults.MaxBytes, s.quotaDefaults.MaxTraces).Scan(
			&v.Overrides.MaxBytes, &v.Overrides.MaxTraces, &v.Version, &v.UpdatedAt, &v.Defaults.MaxBytes, &v.Defaults.MaxTraces,
			&v.Storage.Traces, &v.Storage.Bytes, &v.Storage.NextExpiry)
	}
	if errors.Is(err, pgx.ErrNoRows) && user != "" {
		return v, ErrQuotaUser
	}
	v.Effective = v.Defaults
	if v.Overrides.MaxBytes != nil {
		v.Effective.MaxBytes = *v.Overrides.MaxBytes
	}
	if v.Overrides.MaxTraces != nil {
		v.Effective.MaxTraces = *v.Overrides.MaxTraces
	}
	if v.Storage != nil {
		v.Storage.MaxBytes, v.Storage.MaxTraces = v.Effective.MaxBytes, v.Effective.MaxTraces
	}
	return v, err
}

// SaveQuotas serializes changes across replicas and audits them atomically.
// Quota changes never alter capture consent, retention or stored content.
func (s *Service) SaveQuotas(ctx context.Context, user string, expected int64, q QuotaOverride, actor audit.Actor) error {
	if expected < 0 {
		return ErrQuotaConflict
	}
	if err := validateQuota(q); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old QuotaOverride
		var version int64
		if user == "" {
			if err := tx.QueryRow(ctx, `SELECT max_bytes,max_traces,version FROM trace_quota_defaults WHERE singleton FOR UPDATE`).Scan(&old.MaxBytes, &old.MaxTraces, &version); err != nil {
				return err
			}
		} else {
			// Match the writer's lock order: global row, then account row.
			var singleton bool
			if err := tx.QueryRow(ctx, `SELECT singleton FROM trace_quota_defaults WHERE singleton FOR SHARE`).Scan(&singleton); err != nil {
				return err
			}
			var id string
			if err := tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id=$1 FOR UPDATE`, user).Scan(&id); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrQuotaUser
				}
				return err
			}
			err := tx.QueryRow(ctx, `SELECT max_bytes,max_traces,version FROM trace_quota_overrides WHERE user_id=$1`, user).Scan(&old.MaxBytes, &old.MaxTraces, &version)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if version != expected {
			return ErrQuotaConflict
		}
		equal := func(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		if equal(old.MaxBytes, q.MaxBytes) && equal(old.MaxTraces, q.MaxTraces) {
			return nil
		}
		version++
		action, targetType, target := "trace.quota.defaults", "trace_quotas", "defaults"
		if user == "" {
			if _, err := tx.Exec(ctx, `UPDATE trace_quota_defaults SET max_bytes=$1,max_traces=$2,version=$3,updated_at=clock_timestamp(),updated_by=NULLIF($4,'')::uuid WHERE singleton`, q.MaxBytes, q.MaxTraces, version, actor.ID); err != nil {
				return err
			}
		} else {
			action, targetType, target = "trace.quota.user", "user", user
			if _, err := tx.Exec(ctx, `INSERT INTO trace_quota_overrides(user_id,max_bytes,max_traces,version,updated_by) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid)
				ON CONFLICT(user_id) DO UPDATE SET max_bytes=EXCLUDED.max_bytes,max_traces=EXCLUDED.max_traces,version=EXCLUDED.version,updated_at=clock_timestamp(),updated_by=EXCLUDED.updated_by`, user, q.MaxBytes, q.MaxTraces, version, actor.ID); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{Action: action, Actor: actor, TargetType: targetType, TargetID: target, Result: audit.Succeeded,
			Details: map[string]any{"max_bytes": q.MaxBytes, "max_traces": q.MaxTraces, "version": version}})
	})
}
