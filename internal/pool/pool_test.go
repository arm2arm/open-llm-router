package pool

import (
	"context"
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
