package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func (o *Observer) registerDiagnostics(options Options) {
	gauge := func(name, help string, value func() float64) {
		o.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, value))
	}
	if options.DatabaseHealth != nil {
		gauge("olr_database_reachable", "Database ping success at scrape time, bounded to one second.", func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return boolMetric(options.DatabaseHealth(ctx))
		})
	}
	if options.InvalidationHealth != nil {
		gauge("olr_invalidation_listener_connected", "Whether authentication invalidation is connected and usable.", func() float64 { return boolMetric(options.InvalidationHealth()) })
	}
	if stats := options.GroupSync; stats != nil {
		gauge("olr_group_sync_enabled", "Whether group synchronization is configured.", func() float64 { return boolMetric(stats.Enabled.Load()) })
		gauge("olr_group_sync_interval_seconds", "Configured group synchronization interval.", func() float64 { return float64(stats.IntervalSeconds.Load()) })
		gauge("olr_group_sync_last_success_timestamp_seconds", "Unix time of the last complete sync; zero until success with current settings.", func() float64 { return float64(stats.LastSuccess.Load()) })
		for result, count := range map[string]func() uint64{"success": stats.Success.Load, "error": stats.Failure.Load, "skipped": stats.Skipped.Load} {
			o.Registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "olr_group_sync_runs_total", Help: "Group sync runs and initialization failures by result.", ConstLabels: prometheus.Labels{"result": result}}, func() float64 { return float64(count()) }))
		}
	}
}
