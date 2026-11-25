// Package pool balances requests across healthy replicas, with an optional
// priority queue when every replica is at max_inflight.
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"hash/fnv"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
)

const (
	CheckInterval     = 5 * time.Second
	CheckTimeout      = 2 * time.Second
	Cooldown          = 10 * time.Second
	MaxSessionIDBytes = 256
	// CatalogInterval is how often a healthy replica's model catalog is read
	// again for its context length. It is also read on a replica's first
	// successful check and when it recovers.
	CatalogInterval = 10 * time.Minute
	// CatalogPath is the OpenAI-compatible model catalog.
	CatalogPath     = "/v1/models"
	MaxCatalogBytes = 1 << 20
)

type replica struct {
	config    config.Replica
	inflight  int
	capacity  *atomic.Int64 // shared by routing revisions for the same model/origin
	healthy   bool
	failures  int
	coolUntil time.Time
	// contextLength is what the replica's catalog reports for the model, 0
	// when unknown; catalogAt is when the catalog was last read.
	contextLength int
	catalogAt     time.Time
}

func (r *replica) count() int {
	if r.capacity != nil {
		return int(r.capacity.Load())
	}
	return r.inflight
}

// Pool serializes selection and reservation, so max_inflight cannot be exceeded.
// Network operations never hold its mutex. Callers that may wait are queued
// in priority order and receive a lease directly when one frees up.
type Pool struct {
	mu       sync.Mutex
	replicas []replica
	waiters  []*waiter // ordered by priority, then arrival
	events   *capacityEvents
	upstream string // model name sent to the backends, looked up in their catalogs
}

// A release wakes queued requests across both active and draining revisions.
// Channels avoid retaining old pools once their requests have finished.
type capacityEvents struct {
	mu      sync.Mutex
	changed chan struct{}
}

func (e *capacityEvents) channel() <-chan struct{} {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.changed
}

func (e *capacityEvents) notify() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	close(e.changed)
	e.changed = make(chan struct{})
}

type Lease struct {
	Replica config.Replica
	index   int
	pool    *Pool
	once    sync.Once
}

// Request describes what a caller wants from a pool.
type Request struct {
	Allowed func(config.Replica) bool
	// Exclude is the failed lease of a first attempt, or nil.
	Exclude *Lease
	// Session, if not empty, prefers one replica per session (rendezvous
	// hashing) while it is healthy and has capacity.
	Session    string
	sessionKey [32]byte
	hasSession bool
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
	if ctx.Err() != nil {
		return nil
	}
	// Bound and hash client input once, before taking the selection mutex.
	if r.Session != "" && len(r.Session) <= MaxSessionIDBytes {
		r.sessionKey, r.hasSession = sha256.Sum256([]byte(r.Session)), true
	}
	r.Session = ""
	// Failover is an immediate attempt on another replica, never a queued replay.
	if r.Exclude != nil {
		r.Wait = 0
	}
	p.mu.Lock()
	changed := p.events.channel()
	// Give existing callers first use of capacity they can actually use.
	// An unserviceable waiter must not block unrelated free replicas.
	p.dispatchBefore(r.Priority)
	if lease := p.reserve(r); lease != nil || r.Wait <= 0 {
		p.mu.Unlock()
		return lease
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
waiting:
	for {
		select {
		case lease := <-w.lease:
			if ctx.Err() != nil {
				lease.Release()
				return nil
			}
			return lease
		case <-changed:
			p.mu.Lock()
			changed = p.events.channel()
			// One dispatch serves every waiter that fits, in queue order, so
			// only the head runs it. A notification wakes all waiters; letting
			// each of them dispatch would repeat that scan once per waiter.
			// A departing head dispatches and notifies again (below).
			if len(p.waiters) > 0 && p.waiters[0] == w {
				p.dispatch()
			}
			p.mu.Unlock()
		case <-timer.C:
			break waiting
		case <-ctx.Done():
			break waiting
		}
	}
	p.mu.Lock()
	p.waiters = slices.DeleteFunc(p.waiters, func(other *waiter) bool { return other == w })
	p.dispatch()
	p.mu.Unlock()
	p.events.notify()
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
	p.dispatchBefore(math.MaxInt)
}

func (p *Pool) dispatchBefore(priority int) {
	for i := 0; i < len(p.waiters); {
		w := p.waiters[i]
		if w.priority > priority {
			return
		}
		lease := p.reserve(w.request)
		if lease == nil {
			i++
			continue
		}
		p.waiters = slices.Delete(p.waiters, i, i+1)
		w.lease <- lease
	}
}

// eligible runs the cheap state checks before the caller's access filter: a
// full pool is rescanned for every queued caller on each release.
func (p *Pool) eligible(i int, r Request, now time.Time) bool {
	rep := &p.replicas[i]
	return rep.healthy && !now.Before(rep.coolUntil) && rep.count() < rep.config.MaxInflight &&
		!(r.Exclude != nil && r.Exclude.pool == p && r.Exclude.index == i) && (r.Allowed == nil || r.Allowed(rep.config))
}

// reserve picks a replica for r: the session's preferred replica if it is
// eligible, otherwise the lowest inflight/weight, ties broken at random.
// The caller holds mu.
func (p *Pool) reserve(r Request) *Lease {
	now := time.Now()
	best := -1
	if r.hasSession {
		if i := p.preferred(r.sessionKey); p.eligible(i, r, now) {
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
			s := float64(rep.count()) / rep.config.Weight
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
	rep := &p.replicas[best]
	if rep.capacity != nil {
		for {
			count := rep.capacity.Load()
			if count >= int64(rep.config.MaxInflight) {
				return nil
			}
			if rep.capacity.CompareAndSwap(count, count+1) {
				break
			}
		}
	} else {
		rep.inflight++
	}
	return &Lease{Replica: p.replicas[best].config, index: best, pool: p}
}

// preferred returns the session's replica by rendezvous hashing over replica
// URLs: stable while the pool's configuration is unchanged, and only the
// sessions of a removed replica move. Weights scale the scores.
func (p *Pool) preferred(session [32]byte) int {
	best, top := 0, -1.0
	for i, rep := range p.replicas {
		h := fnv.New64a()
		h.Write([]byte(rep.config.URL))
		h.Write([]byte{0})
		h.Write(session[:])
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
		rep := &l.pool.replicas[l.index]
		if rep.capacity != nil {
			rep.capacity.Add(-1)
		} else {
			rep.inflight--
		}
		l.pool.dispatch()
		l.pool.mu.Unlock()
		l.pool.events.notify()
	})
}

func (l *Lease) Failed() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	l.pool.replicas[l.index].coolUntil = time.Now().Add(Cooldown)
}

// State contains operational metadata only, including no backend credentials.
type State struct {
	Backend     string  `json:"backend,omitempty"`
	URL         string  `json:"url"`
	Inflight    int     `json:"in_flight"`
	Healthy     bool    `json:"healthy"`
	Weight      float64 `json:"weight"`
	MaxInflight int     `json:"max_inflight"`
	// ContextLength is the context length the replica's model catalog
	// reports for the model; omitted when unknown.
	ContextLength int `json:"context_length,omitempty"`
}

func (p *Pool) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]State, len(p.replicas))
	for i, r := range p.replicas {
		out[i] = State{Backend: r.config.Backend, URL: r.config.URL, Inflight: r.count(), Healthy: r.healthy && !time.Now().Before(r.coolUntil),
			Weight: r.config.Weight, MaxInflight: r.config.MaxInflight, ContextLength: r.contextLength}
	}
	return out
}

// ContextLength is the smallest context length that the replicas' model
// catalogs report, so that a request fits whichever replica serves it. It is
// 0 when no replica reports one.
func (p *Pool) ContextLength() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	smallest := 0
	for _, r := range p.replicas {
		if r.contextLength > 0 && (smallest == 0 || r.contextLength < smallest) {
			smallest = r.contextLength
		}
	}
	return smallest
}

// CatalogEntry is the part of an entry of an OpenAI-compatible model catalog
// (GET /v1/models) that the router reads.
type CatalogEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	// MaxModelLen is the context length that vLLM and SGLang report;
	// ContextLength is the name other OpenAI-compatible catalogs use. They
	// stay raw, so an unexpected value cannot make the catalog unreadable.
	MaxModelLen   json.RawMessage `json:"max_model_len"`
	ContextLength json.RawMessage `json:"context_length"`
}

// ReportedContextLength is the entry's context length, or 0 if it reports
// no positive integer up to config.MaxContextLength.
func (e CatalogEntry) ReportedContextLength() int {
	for _, raw := range []json.RawMessage{e.MaxModelLen, e.ContextLength} {
		if n, err := strconv.Atoi(string(raw)); err == nil && n > 0 && n <= config.MaxContextLength {
			return n
		}
	}
	return 0
}

// catalogContextLength reads a model catalog and returns the context length
// it reports for model, 0 if it reports none. ok is false when the body is not
// a readable catalog, so the previous value is kept.
func catalogContextLength(body io.Reader, model string) (n int, ok bool) {
	data, err := io.ReadAll(io.LimitReader(body, MaxCatalogBytes+1))
	if err != nil || len(data) > MaxCatalogBytes {
		return 0, false
	}
	var catalog struct {
		Data *[]CatalogEntry `json:"data"`
	}
	if json.Unmarshal(data, &catalog) != nil || catalog.Data == nil {
		return 0, false
	}
	for _, entry := range *catalog.Data {
		if entry.ID == model {
			return entry.ReportedContextLength(), true
		}
	}
	return 0, true
}

type Manager struct {
	Pools      map[string]*Pool // immutable after construction
	client     *http.Client
	capacities map[string]*atomic.Int64
	// events are per model, like the capacity counters its revisions share:
	// a release wakes only callers that could use the freed capacity.
	events map[string]*capacityEvents
}

func New(cfg *config.Config, transport http.RoundTripper) *Manager {
	return Replace(cfg, transport, nil)
}

// Replace reuses unchanged pools and capacity counters of changed replicas.
// Old leases keep their own immutable destination/credential until release.
func Replace(cfg *config.Config, transport http.RoundTripper, previous *Manager) *Manager {
	m := &Manager{Pools: make(map[string]*Pool), client: &http.Client{Transport: transport, Timeout: CheckTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	m.capacities = map[string]*atomic.Int64{}
	m.events = map[string]*capacityEvents{}
	if previous != nil {
		// Removed models keep their entries: their draining leases still
		// release capacity that a restored revision must observe.
		maps.Copy(m.capacities, previous.capacities)
		maps.Copy(m.events, previous.events)
	}
	for name, model := range cfg.Models {
		if m.events[name] == nil {
			m.events[name] = &capacityEvents{changed: make(chan struct{})}
		}
		upstream := model.UpstreamModel
		if upstream == "" {
			upstream = name
		}
		if previous != nil {
			if old := previous.Pools[name]; old != nil {
				old.mu.Lock()
				same := old.upstream == upstream && len(old.replicas) == len(model.Pool)
				for i, r := range old.replicas {
					same = same && r.config == model.Pool[i]
				}
				old.mu.Unlock()
				if same {
					m.Pools[name] = old
					continue
				}
			}
		}
		p := &Pool{events: m.events[name], upstream: upstream}
		for _, r := range model.Pool {
			key := name + "\x00" + r.URL
			counter := m.capacities[key]
			if counter == nil {
				counter = &atomic.Int64{}
				m.capacities[key] = counter
			}
			p.replicas = append(p.replicas, replica{config: r, capacity: counter})
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

// ContextLength is the smallest context length the replicas of model report,
// or 0 when none does or the model has no pool.
func (m *Manager) ContextLength(model string) int {
	if m == nil || m.Pools[model] == nil {
		return 0
	}
	return m.Pools[model].ContextLength()
}

// Check probes in parallel, returning each probe's success separately from the
// ongoing two-failure health state. Every response is closed, including failures.
// A replica that passes and whose catalog is due also has its model catalog
// read for the model's context length; a health path of /v1/models is that
// catalog, so no second request is made.
func (m *Manager) Check(ctx context.Context) map[string][]bool {
	var wg sync.WaitGroup
	results := make(map[string][]bool, len(m.Pools))
	for name, p := range m.Pools {
		checks := make([]bool, len(p.replicas))
		results[name] = checks
		for i := range p.replicas {
			wg.Go(func() {
				r := p.replicas[i].config
				p.mu.Lock()
				due := p.replicas[i].catalogAt.IsZero() || !p.replicas[i].healthy || time.Since(p.replicas[i].catalogAt) >= CatalogInterval
				p.mu.Unlock()
				path := r.HealthPath
				if path == "" {
					path = "/health"
				}
				probeCtx := ctx
				if r.PrivateNetwork {
					probeCtx = backendpolicy.WithPrivateNetwork(ctx, r.URL)
				}
				req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(r.URL, "/")+path, nil)
				if r.BearerToken != "" {
					req.Header.Set("Authorization", "Bearer "+r.BearerToken)
				}
				resp, err := m.client.Do(req)
				ok := err == nil && resp.StatusCode == http.StatusOK
				checks[i] = ok
				contextLength, read := 0, false
				if ok && due && path == CatalogPath {
					contextLength, read = catalogContextLength(resp.Body, p.upstream)
				}
				if resp != nil {
					resp.Body.Close()
				}
				if ok && due && path != CatalogPath {
					contextLength, read = m.catalogContextLength(probeCtx, r, p.upstream)
				}
				if ctx.Err() != nil {
					return
				}
				p.mu.Lock()
				defer p.mu.Unlock()
				state := &p.replicas[i]
				if ok && due {
					state.catalogAt = time.Now()
					if read {
						state.contextLength = contextLength
					}
				}
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
	return results
}

// catalogContextLength reads a replica's model catalog for model. A failed or
// unreadable catalog returns ok false and keeps the previous value.
func (m *Manager) catalogContextLength(ctx context.Context, r config.Replica, model string) (n int, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.URL, "/")+CatalogPath, nil)
	if err != nil {
		return 0, false
	}
	if r.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.BearerToken)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	return catalogContextLength(resp.Body, model)
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
