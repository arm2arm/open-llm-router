package metrics

import (
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/prometheus/client_golang/prometheus"
)

type poolCollector struct {
	source           func() *pool.Manager
	pools            *pool.Manager
	inflight, health *prometheus.Desc
	queued           *prometheus.Desc
}

func newPoolCollector(pools *pool.Manager) *poolCollector {
	return &poolCollector{pools: pools,
		inflight: prometheus.NewDesc("olr_replica_in_flight", "Calls currently holding a replica lease.", []string{"model", "replica"}, nil),
		health:   prometheus.NewDesc("olr_replica_healthy", "Replica passes health checks and is outside cooldown (1 or 0).", []string{"model", "replica"}, nil),
		queued:   prometheus.NewDesc("olr_queue_waiting", "Calls waiting for a free replica of the model.", []string{"model"}, nil)}
}
func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.inflight
	ch <- c.health
	ch <- c.queued
}
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	pools := c.pools
	if c.source != nil {
		pools = c.source()
	}
	for name, p := range pools.Pools {
		ch <- prometheus.MustNewConstMetric(c.queued, prometheus.GaugeValue, float64(p.Queued()), name)
		for _, r := range p.Snapshot() {
			health := 0.0
			if r.Healthy {
				health = 1
			}
			ch <- prometheus.MustNewConstMetric(c.inflight, prometheus.GaugeValue, float64(r.Inflight), name, r.URL)
			ch <- prometheus.MustNewConstMetric(c.health, prometheus.GaugeValue, health, name, r.URL)
		}
	}
}
