package pool

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func testPool() *Pool {
	return &Pool{replicas: []replica{
		{config: config.Replica{URL: "http://a", Weight: 2, MaxInflight: 400}, healthy: true},
		{config: config.Replica{URL: "http://b", Weight: 1, MaxInflight: 400}, healthy: true},
	}}
}

func TestWeightedSelection(t *testing.T) {
	p := testPool()
	var counts [2]int
	var leases []*Lease
	for range 300 {
		l := p.Acquire(nil)
		counts[l.index]++
		leases = append(leases, l)
	}
	if counts != [2]int{200, 100} {
		t.Fatalf("2:1 weighted selection = %v", counts)
	}
	// Keep load in flight, then repeatedly complete and replace it.
	counts = [2]int{}
	for i := range 3000 {
		j := i % len(leases)
		leases[j].Release()
		l := p.Acquire(nil)
		counts[l.index]++
		leases[j] = l
	}
	if counts[0] < 1900 || counts[0] > 2100 {
		t.Fatalf("sustained split = %v", counts)
	}
	for _, l := range leases {
		l.Release()
		l.Release()
	}
	for _, s := range p.Snapshot() {
		if s.Inflight != 0 {
			t.Fatal(s)
		}
	}
}

func TestReplicaAccessFilterAlsoAppliesToAffinityAndRetry(t *testing.T) {
	p := testPool()
	p.replicas[0].config.Backend = "staff"
	p.replicas[1].config.Backend = "students"
	allowed := func(replica config.Replica) bool { return replica.Backend == "students" }
	for range 30 {
		lease := p.AcquireRequest(t.Context(), Request{Session: "same-session", Allowed: allowed})
		if lease == nil || lease.Replica.Backend != "students" {
			t.Fatal("selected unauthorized backend")
		}
		retry := p.AcquireRequest(t.Context(), Request{Exclude: lease, Allowed: allowed})
		if retry != nil {
			retry.Release()
			t.Fatal("retry bypassed backend access")
		}
		lease.Release()
	}
}

func TestConcurrentCapsAndExclusion(t *testing.T) {
	p := testPool()
	for i := range p.replicas {
		p.replicas[i].config.MaxInflight = 3
	}
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			for range 10 {
				l := p.Acquire(nil)
				if l == nil {
					continue
				}
				for _, s := range p.Snapshot() {
					if s.Inflight > 3 {
						t.Errorf("cap exceeded: %v", s)
					}
				}
				time.Sleep(time.Millisecond)
				l.Release()
			}
		})
	}
	wg.Wait()
	l := p.Acquire(nil)
	l.Failed()
	l.Release()
	next := p.Acquire(l)
	if next == nil || next.index == l.index {
		t.Fatal("retry chose failed replica")
	}
	next.Release()
	for _, s := range p.Snapshot() {
		if s.Inflight != 0 {
			t.Fatal(s)
		}
	}
}

func TestHealthThresholdsAndCooldown(t *testing.T) {
	var failing atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer backend-key" {
			t.Error("bad probe", r.URL, r.Header)
		}
		if failing.Load() {
			w.WriteHeader(503)
		}
	}))
	defer backend.Close()
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: backend.URL + "/", Weight: 1, MaxInflight: 1, BearerToken: "backend-key"}}}}}
	m := New(cfg, http.DefaultTransport)
	ctx := t.Context()
	if m.Ready() {
		t.Fatal("unchecked replica marked ready")
	}
	m.Check(ctx)
	if !m.Ready() {
		t.Fatal("success did not make ready")
	}
	failing.Store(true)
	m.Check(ctx)
	if !m.Ready() {
		t.Fatal("single failure marked unhealthy")
	}
	m.Check(ctx)
	if m.Ready() || m.Pools["m"].Acquire(nil) != nil {
		t.Fatal("two failures did not remove replica")
	}
	failing.Store(false)
	m.Check(ctx)
	if !m.Ready() {
		t.Fatal("one success did not restore replica")
	}
	l := m.Pools["m"].Acquire(nil)
	l.Failed()
	l.Release()
	m.Check(ctx)
	if m.Ready() {
		t.Fatal("probe success overrode passive cooldown")
	}
	p := m.Pools["m"]
	p.mu.Lock()
	p.replicas[0].coolUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if !m.Ready() {
		t.Fatal("expired cooldown not restored")
	}
}

func TestHealthLoopStops(t *testing.T) {
	m := New(&config.Config{}, http.DefaultTransport)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health loop leaked")
	}
}

func TestCustomHealthPath(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	cfg := &config.Config{Models: map[string]config.Model{"ollama": {Pool: []config.Replica{{URL: backend.URL, HealthPath: "/", Weight: 1, MaxInflight: 1}}}}}
	m := New(cfg, http.DefaultTransport)
	m.Check(t.Context())
	if !m.Ready() {
		t.Fatal("backend with a custom health path did not become ready")
	}
}

func fullPool(max int) *Pool {
	return &Pool{replicas: []replica{
		{config: config.Replica{URL: "http://a", Weight: 1, MaxInflight: max}, healthy: true},
		{config: config.Replica{URL: "http://b", Weight: 1, MaxInflight: max}, healthy: true},
	}}
}

func waitQueued(t *testing.T, p *Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.Queued() != n {
		if time.Now().After(deadline) {
			t.Fatal("queue length", p.Queued())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestQueueAbsorbsShortOverloadAndTimesOut(t *testing.T) {
	p := fullPool(1)
	a, b := p.Acquire(nil), p.Acquire(nil)
	if p.AcquireRequest(t.Context(), Request{}) != nil {
		t.Fatal("a full pool admitted a call without waiting")
	}
	got := make(chan *Lease)
	go func() { got <- p.AcquireRequest(t.Context(), Request{Wait: 2 * time.Second}) }()
	waitQueued(t, p, 1)
	a.Release()
	lease := <-got
	if lease == nil || lease.index != a.index {
		t.Fatal("the freed replica did not go to the waiter", lease)
	}
	started := time.Now()
	if p.AcquireRequest(t.Context(), Request{Wait: 50 * time.Millisecond}) != nil || time.Since(started) < 50*time.Millisecond {
		t.Fatal("a long overload must end in nil after the wait")
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { got <- p.AcquireRequest(ctx, Request{Wait: time.Minute}) }()
	waitQueued(t, p, 1)
	cancel()
	if <-got != nil || p.Queued() != 0 {
		t.Fatal("a cancelled waiter must leave the queue")
	}
	lease.Release()
	b.Release()
	for _, s := range p.Snapshot() {
		if s.Inflight != 0 {
			t.Fatal("leaked lease", s)
		}
	}
}

func TestInteractiveIsServedBeforeBatch(t *testing.T) {
	p := fullPool(1)
	held := []*Lease{p.Acquire(nil), p.Acquire(nil)}
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	enqueue := func(priority int) {
		wg.Go(func() {
			l := p.AcquireRequest(t.Context(), Request{Priority: priority, Wait: 5 * time.Second})
			mu.Lock()
			order = append(order, priority)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			l.Release()
		})
	}
	// Batch callers arrive first; interactive ones still go ahead of them.
	for i, priority := range []int{10, 10, 0, 0} {
		enqueue(priority)
		waitQueued(t, p, i+1)
	}
	// A caller that does not wait cannot overtake the queue either.
	if p.AcquireRequest(t.Context(), Request{Priority: 10}) != nil {
		t.Fatal("a new caller overtook the queue")
	}
	held[0].Release()
	wg.Wait()
	held[1].Release()
	if len(order) != 4 || order[0] != 0 || order[1] != 0 || order[2] != 10 {
		t.Fatalf("service order %v", order)
	}
}

func TestSessionAffinity(t *testing.T) {
	p := &Pool{}
	for _, u := range []string{"http://a", "http://b", "http://c", "http://d"} {
		p.replicas = append(p.replicas, replica{config: config.Replica{URL: u, Weight: 1, MaxInflight: 2}, healthy: true})
	}
	homes := map[int]int{}
	for i := range 200 {
		session := fmt.Sprintf("session-%d", i)
		first := p.AcquireRequest(t.Context(), Request{Session: session})
		second := p.AcquireRequest(t.Context(), Request{Session: session})
		if first.index != second.index {
			t.Fatal("one session landed on two replicas while its home had capacity")
		}
		homes[first.index]++
		// The home is full now; the session spills to another replica.
		third := p.AcquireRequest(t.Context(), Request{Session: session})
		if third == nil || third.index == first.index {
			t.Fatal("a full home replica must not take more")
		}
		first.Release()
		second.Release()
		third.Release()
	}
	for i := range 4 {
		if homes[i] < 25 {
			t.Fatalf("sessions are unevenly spread: %v", homes)
		}
	}
	home := p.preferred("sticky")
	p.replicas[home].healthy = false
	l := p.AcquireRequest(t.Context(), Request{Session: "sticky"})
	if l == nil || l.index == home {
		t.Fatal("an unhealthy home was used")
	}
	l.Release()
}
