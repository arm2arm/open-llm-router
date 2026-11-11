package loadtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type RuntimeSnapshot struct {
	At                                                         time.Time
	CPUSeconds, CPUCores, ResidentBytes, HeapInUse, Goroutines float64
	ReplicaInFlight, UserInFlight, WriteFailures, Dropped      float64
	RequestsByReplica                                          map[string]float64
}

func (r *runner) get(ctx context.Context, endpoint, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("invalid observation endpoint")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := r.probes.Do(req)
	if err != nil {
		return nil, errors.New("observation request failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("observation returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (r *runner) snapshot(ctx context.Context) (RuntimeSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s := RuntimeSnapshot{At: time.Now().UTC(), RequestsByReplica: map[string]float64{}}
	resp, err := r.get(ctx, r.o.Router+"/metrics", r.o.MetricsToken)
	if err != nil {
		return s, err
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return s, errors.New("invalid Prometheus response")
	}
	for name, dest := range map[string]*float64{
		"process_cpu_seconds_total": &s.CPUSeconds, "process_resident_memory_bytes": &s.ResidentBytes,
		"go_memstats_heap_inuse_bytes": &s.HeapInUse, "go_goroutines": &s.Goroutines,
		"olr_replica_in_flight": &s.ReplicaInFlight, "olr_usage_write_failures_total": &s.WriteFailures, "olr_usage_dropped_total": &s.Dropped,
	} {
		family, ok := families[name]
		if !ok {
			return s, fmt.Errorf("required metric %s is missing", name)
		}
		for _, metric := range family.Metric {
			*dest += metric.GetGauge().GetValue() + metric.GetCounter().GetValue()
		}
	}
	if family := families["olr_requests_total"]; family != nil {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "replica" && label.GetValue() != "" {
					s.RequestsByReplica[label.GetValue()] += metric.GetCounter().GetValue()
				}
			}
		}
	}
	resp, err = r.get(ctx, r.o.Router+"/v1/me/usage", r.o.APIKey)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	var state struct {
		InFlight *float64 `json:"in_flight"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&state) != nil || state.InFlight == nil {
		return s, errors.New("invalid current-user usage response")
	}
	s.UserInFlight = *state.InFlight
	return s, nil
}

func (r *runner) monitor(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var previous RuntimeSnapshot
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s, err := r.snapshot(ctx)
			if err != nil {
				continue
			}
			if !previous.At.IsZero() {
				r.peak.CPUCores = max(r.peak.CPUCores, (s.CPUSeconds-previous.CPUSeconds)/s.At.Sub(previous.At).Seconds())
			}
			previous = s
			r.peak.Samples++
			r.peak.ResidentBytes = max(r.peak.ResidentBytes, s.ResidentBytes)
			r.peak.HeapInUse = max(r.peak.HeapInUse, s.HeapInUse)
			r.peak.Goroutines = max(r.peak.Goroutines, s.Goroutines)
			r.peak.ReplicaInFlight = max(r.peak.ReplicaInFlight, s.ReplicaInFlight)
			r.peak.UserInFlight = max(r.peak.UserInFlight, s.UserInFlight)
		}
	}
}

type mockSnapshot struct {
	Requests int `json:"requests"`
	Aborted  int `json:"streams_aborted"`
	InFlight int `json:"inflight"`
}

func (r *runner) mockStats(ctx context.Context) ([]mockSnapshot, error) {
	if !r.o.Mock {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stats := make([]mockSnapshot, len(r.o.Direct))
	for i, endpoint := range r.o.Direct {
		resp, err := r.get(ctx, endpoint+"/_mock/stats", "")
		if err != nil {
			return nil, err
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&stats[i])
		resp.Body.Close()
		if err != nil {
			return nil, errors.New("invalid mock statistics")
		}
	}
	return stats, nil
}

type accounting struct {
	pool   *pgxpool.Pool
	keyID  string
	before int64
}

func (r *runner) openAccounting(ctx context.Context) error {
	if r.o.DatabaseURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(r.o.DatabaseURL)
	if err != nil {
		return errors.New("invalid OLR_LOADTEST_DATABASE_URL")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("create load-test database connection")
	}
	a := &accounting{pool: pool}
	hash := sha256.Sum256([]byte(r.o.APIKey))
	if err := pool.QueryRow(ctx, `SELECT id::text FROM api_keys WHERE hash=$1`, hash[:]).Scan(&a.keyID); err != nil {
		pool.Close()
		return errors.New("cannot find the load-test API key in the database")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage WHERE key_id=$1::uuid`, a.keyID).Scan(&a.before); err != nil {
		pool.Close()
		return errors.New("cannot read usage baseline")
	}
	r.account = a
	return nil
}

// idle polls bounded observations until cancellations and asynchronous writes
// settle. Closing client keep-alives makes goroutine snapshots comparable.
func (r *runner) idle(ctx context.Context) (RuntimeSnapshot, error) {
	r.load.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var settled time.Time
	for {
		s, err := r.snapshot(ctx)
		if err != nil {
			return s, err
		}
		stats, err := r.mockStats(ctx)
		if err != nil {
			return s, err
		}
		idle := s.ReplicaInFlight == 0 && s.UserInFlight == 0
		for _, stat := range stats {
			idle = idle && stat.InFlight == 0
		}
		if r.account != nil {
			var count int64
			if err := r.account.pool.QueryRow(ctx, `SELECT count(*) FROM usage WHERE key_id=$1::uuid`, r.account.keyID).Scan(&count); err != nil {
				return s, errors.New("cannot read usage count")
			}
			r.report.UsageRows = count - r.account.before
			idle = idle && (r.report.UsageRows == r.report.RouterRequests || s.Dropped > 0 || s.WriteFailures > 0)
		}
		if idle {
			if settled.IsZero() {
				settled = time.Now()
			}
			if time.Since(settled) >= r.o.Settle {
				return s, nil
			}
		} else {
			settled = time.Time{}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return s, errors.New("load did not drain within 30 seconds (leases, concurrency slots, or accounting)")
		case <-timer.C:
		}
	}
}
