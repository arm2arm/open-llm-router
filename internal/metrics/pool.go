package metrics

import (
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/prometheus/client_golang/prometheus"
)

type poolCollector struct {
	pools            *pool.Manager
	inflight, health *prometheus.Desc
}

func newPoolCollector(pools *pool.Manager) *poolCollector {
	return &poolCollector{pools: pools,
		inflight: prometheus.NewDesc("olr_replica_in_flight", "Calls currently holding a replica lease.", []string{"model", "replica"}, nil),
		health:   prometheus.NewDesc("olr_replica_healthy", "Replica passes health checks and is outside cooldown (1 or 0).", []string{"model", "replica"}, nil)}
}
func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.inflight; ch <- c.health }
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	for name, p := range c.pools.Pools {
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
