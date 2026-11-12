package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/pool"
)

type routingSnapshot struct {
	config *config.Config
	pools  *pool.Manager
}

// Snapshot is pinned once per inference request; callers never mutate it.
func (h *Handler) Snapshot() (*config.Config, *pool.Manager) {
	if state := h.routing.Load(); state != nil {
		return state.config, state.pools
	}
	return h.cfg, h.Pools
}

func (h *Handler) CurrentPools() *pool.Manager { _, pools := h.Snapshot(); return pools }

// ConfigureDestinations is installed before serving or probing endpoints.
func (h *Handler) ConfigureDestinations(policy *backendpolicy.Policy) {
	h.transport.DialContext = policy.DialContext
	// Explicit HTTP(S) endpoints use the destination policy directly, independent
	// of proxy environment variables that could bypass checked IP dialing.
	h.transport.Proxy = nil
}

// Prepare tests a replacement without changing the active snapshot. The returned
// publication cannot fail and is called only after the database commit succeeds.
func (h *Handler) Prepare(ctx context.Context, cfg *config.Config, model string) (func(), []pool.State, error) {
	_, previous := h.Snapshot()
	next := pool.Replace(cfg, h.transport, previous)
	checks := next.Check(ctx)[model]
	states := next.Pools[model].Snapshot()
	for i := range states {
		states[i].Healthy = states[i].Healthy && checks[i]
	}
	for _, state := range states {
		if !state.Healthy {
			return nil, states, errors.New("one or more replicas failed their health check; verify origin, health path and credential reference")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, states, errors.New("endpoint check timed out")
	}
	return func() { h.routing.Store(&routingSnapshot{cfg, next}) }, states, nil
}

// RunHealth probes the currently published pool set, retaining all old leases
// until their requests finish without restarting admission or usage accounting.
func (h *Handler) RunHealth(ctx context.Context) {
	ticker := time.NewTicker(pool.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.CurrentPools().Check(ctx)
		}
	}
}
