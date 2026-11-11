// Package pool balances requests across healthy replicas, with an optional
// priority queue when every replica is at max_inflight.
package pool

import (
	"context"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
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
// Network operations never hold its mutex. Callers that may wait are queued
// in priority order and receive a lease directly when one frees up.
type Pool struct {
	mu       sync.Mutex
	replicas []replica
	waiters  []*waiter // ordered by priority, then arrival
}

type Lease struct {
	Replica config.Replica
	index   int
	pool    *Pool
	once    sync.Once
}

// Request describes what a caller wants from a pool.
type Request struct {
	// Exclude is the failed lease of a first attempt, or nil.
	Exclude *Lease
	// Session, if not empty, prefers one replica per session (rendezvous
	// hashing) while it is healthy and has capacity.
	Session string
	// Priority orders queued callers: lower values are served first.
	Priority int
	// Wait is how long to queue when every replica is full; 0 does not wait.
	Wait time.Duration
}

type waiter struct {
	request  Request
	lease    chan *Lease // buffered; receives at most one lease
	priority int
}

// Acquire reserves the least outstanding/weight replica without waiting;
// exclude is the failed lease from the first attempt, or nil.
func (p *Pool) Acquire(exclude *Lease) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reserve(Request{Exclude: exclude})
}

// AcquireRequest reserves a replica, queueing up to r.Wait while the pool is
// full. It returns nil when the wait ends without capacity or ctx is done.
// Callers already queued with the same or a better priority go first.
func (p *Pool) AcquireRequest(ctx context.Context, r Request) *Lease {
	p.mu.Lock()
	ahead := false
	for _, w := range p.waiters {
		ahead = ahead || w.priority <= r.Priority
	}
	if !ahead {
		if lease := p.reserve(r); lease != nil || r.Wait <= 0 {
			p.mu.Unlock()
			return lease
		}
	} else if r.Wait <= 0 {
		p.mu.Unlock()
		return nil
	}
	w := &waiter{request: r, lease: make(chan *Lease, 1), priority: r.Priority}
	at := len(p.waiters)
	for i, other := range p.waiters {
		if other.priority > w.priority {
			at = i
			break
		}
	}
	p.waiters = slices.Insert(p.waiters, at, w)
	p.mu.Unlock()
	timer := time.NewTimer(r.Wait)
	defer timer.Stop()
	select {
	case lease := <-w.lease:
		return lease
	case <-timer.C:
	case <-ctx.Done():
	}
	p.mu.Lock()
	p.waiters = slices.DeleteFunc(p.waiters, func(other *waiter) bool { return other == w })
	p.mu.Unlock()
	// A lease may have been handed over just before the waiter left.
	select {
	case lease := <-w.lease:
		if ctx.Err() == nil {
			return lease
		}
		lease.Release()
	default:
	}
	return nil
}

// Queued is the number of callers waiting for this pool.
func (p *Pool) Queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.waiters)
}

// dispatch hands free capacity to queued callers in order. The caller holds mu.
func (p *Pool) dispatch() {
	for len(p.waiters) > 0 {
		w := p.waiters[0]
		lease := p.reserve(w.request)
		if lease == nil {
			return
		}
		p.waiters = p.waiters[1:]
		w.lease <- lease
	}
}

func (p *Pool) eligible(i int, r Request, now time.Time) bool {
	rep := &p.replicas[i]
	return !(r.Exclude != nil && r.Exclude.pool == p && r.Exclude.index == i) && rep.healthy && !now.Before(rep.coolUntil) && rep.inflight < rep.config.MaxInflight
}

// reserve picks a replica for r: the session's preferred replica if it is
// eligible, otherwise the lowest inflight/weight, ties broken at random.
// The caller holds mu.
func (p *Pool) reserve(r Request) *Lease {
	now := time.Now()
	best := -1
	if r.Session != "" {
		if i := p.preferred(r.Session); p.eligible(i, r, now) {
			best = i
		}
	}
	if best == -1 {
		ties, score := 0, 0.0
		for i := range p.replicas {
			if !p.eligible(i, r, now) {
				continue
			}
			rep := &p.replicas[i]
			s := float64(rep.inflight) / rep.config.Weight
			if best == -1 || s < score {
				best, ties, score = i, 1, s
			} else if s == score {
				ties++
				if rand.IntN(ties) == 0 {
					best = i
				}
			}
		}
	}
	if best == -1 {
		return nil
	}
	p.replicas[best].inflight++
	return &Lease{Replica: p.replicas[best].config, index: best, pool: p}
}

// preferred returns the session's replica by rendezvous hashing over replica
// URLs: stable while the pool's configuration is unchanged, and only the
// sessions of a removed replica move. Weights scale the scores.
func (p *Pool) preferred(session string) int {
	best, top := 0, -1.0
	for i, rep := range p.replicas {
		h := fnv.New64a()
		h.Write([]byte(rep.config.URL))
		h.Write([]byte{0})
		h.Write([]byte(session))
		// Weighted rendezvous: -w / ln(u) with u uniform in (0, 1).
		u := (float64(mix(h.Sum64())>>11) + 0.5) / (1 << 53)
		score := -rep.config.Weight / math.Log(u)
		if score > top {
			best, top = i, score
		}
	}
	return best
}

// mix is the splitmix64 finalizer: FNV alone spreads its last input bytes
// poorly into the high bits used above.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		defer l.pool.mu.Unlock()
		l.pool.replicas[l.index].inflight--
		l.pool.dispatch()
	})
}

func (l *Lease) Failed() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	l.pool.replicas[l.index].coolUntil = time.Now().Add(Cooldown)
}

// State contains operational metadata only, including no backend credentials.
type State struct {
	URL         string  `json:"url"`
	Inflight    int     `json:"in_flight"`
	Healthy     bool    `json:"healthy"`
	Weight      float64 `json:"weight"`
	MaxInflight int     `json:"max_inflight"`
}

func (p *Pool) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, len(p.replicas))
	for i, r := range p.replicas {
		out[i] = State{URL: r.config.URL, Inflight: r.inflight, Healthy: r.healthy && !time.Now().Before(r.coolUntil),
			Weight: r.config.Weight, MaxInflight: r.config.MaxInflight}
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
				path := r.HealthPath
				if path == "" {
					path = "/health"
				}
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.URL, "/")+path, nil)
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
					p.dispatch()
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
