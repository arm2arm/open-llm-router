// Package pool balances requests across healthy replicas without queueing.
package pool

import (
	"context"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

const (
	CheckInterval = 5 * time.Second
	CheckTimeout  = 2 * time.Second
	Cooldown      = 10 * time.Second
)

type replica struct {
	config    config.Replica
	inflight  int
	healthy   bool
	failures  int
	coolUntil time.Time
}

// Pool serializes selection and reservation, so max_inflight cannot be exceeded.
// Network operations never hold its mutex.
type Pool struct {
	mu       sync.Mutex
	replicas []replica
}

type Lease struct {
	Replica config.Replica
	index   int
	pool    *Pool
	once    sync.Once
}

// Acquire reserves the least outstanding/weight replica; exclude is the failed
// lease from the first attempt, or nil. Ties use reservoir sampling.
func (p *Pool) Acquire(exclude *Lease) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	best, ties, score := -1, 0, 0.0
	now := time.Now()
	for i := range p.replicas {
		r := &p.replicas[i]
		if (exclude != nil && exclude.pool == p && exclude.index == i) || !r.healthy || now.Before(r.coolUntil) || r.inflight >= r.config.MaxInflight {
			continue
		}
		s := float64(r.inflight) / r.config.Weight
		if best == -1 || s < score {
			best, ties, score = i, 1, s
		} else if s == score {
			ties++
			if rand.IntN(ties) == 0 {
				best = i
			}
		}
	}
	if best == -1 {
		return nil
	}
	p.replicas[best].inflight++
	return &Lease{Replica: p.replicas[best].config, index: best, pool: p}
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		l.pool.replicas[l.index].inflight--
	})
}

func (l *Lease) Failed() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	l.pool.replicas[l.index].coolUntil = time.Now().Add(Cooldown)
}

// State contains operational metadata only, including no backend credentials.
type State struct {
	URL      string
	Inflight int
	Healthy  bool
}

func (p *Pool) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, len(p.replicas))
	for i, r := range p.replicas {
		out[i] = State{r.config.URL, r.inflight, r.healthy && !time.Now().Before(r.coolUntil)}
	}
	return out
}

type Manager struct {
	Pools  map[string]*Pool // immutable after construction
	client *http.Client
}

func New(cfg *config.Config, transport http.RoundTripper) *Manager {
	m := &Manager{Pools: make(map[string]*Pool), client: &http.Client{Transport: transport, Timeout: CheckTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	for name, model := range cfg.Models {
		p := &Pool{}
		for _, r := range model.Pool {
			p.replicas = append(p.replicas, replica{config: r})
		}
		m.Pools[name] = p
	}
	return m
}

func (m *Manager) Ready() bool {
	for _, p := range m.Pools {
		healthy := false
		for _, s := range p.Snapshot() {
			healthy = healthy || s.Healthy
		}
		if !healthy {
			return false
		}
	}
	return true
}

// Check probes in parallel. Every response is closed, including failures.
func (m *Manager) Check(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range m.Pools {
		for i := range p.replicas {
			wg.Go(func() {
				r := p.replicas[i].config
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.URL, "/")+"/health", nil)
				if r.BearerToken != "" {
					req.Header.Set("Authorization", "Bearer "+r.BearerToken)
				}
				resp, err := m.client.Do(req)
				ok := err == nil && resp.StatusCode == http.StatusOK
				if resp != nil {
					resp.Body.Close()
				}
				if ctx.Err() != nil {
					return
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				state := &p.replicas[i]
				if ok {
					state.healthy, state.failures = true, 0
				} else {
					state.failures++
					if state.failures >= 2 {
						state.healthy = false
					}
				}
			})
		}
	}
	wg.Wait()
}

// Run performs ongoing health checks. Call Check once before serving traffic.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}
