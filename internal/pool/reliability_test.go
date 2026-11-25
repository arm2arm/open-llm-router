package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestExcludedReplicaNeverQueuesRetry(t *testing.T) {
	for _, size := range []int{1, 2} {
		p := fullPool(1)
		p.replicas = p.replicas[:size]
		held := p.Acquire(nil)
		var other *Lease
		if size == 2 {
			other = p.Acquire(nil)
			defer other.Release()
		}
		held.Release()
		start := time.Now()
		if l := p.AcquireRequest(t.Context(), Request{Exclude: held, Wait: time.Second}); l != nil {
			l.Release()
			t.Fatal("retry used the excluded replica")
		}
		if time.Since(start) > 100*time.Millisecond || p.Queued() != 0 {
			t.Fatal("retry queued")
		}
		l := p.AcquireRequest(t.Context(), Request{})
		if l == nil || l.index != held.index {
			t.Fatal("retry blocked a fresh caller")
		}
		l.Release()
	}
}

func TestUnserviceableWaiterDoesNotBlockCapacity(t *testing.T) {
	for _, wake := range []string{"release", "cancel", "timeout", "new caller"} {
		t.Run(wake, func(t *testing.T) {
			p := fullPool(1)
			a, b := p.Acquire(nil), p.Acquire(nil)
			defer a.Release()
			defer b.Release()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wait := time.Second
			if wake == "timeout" {
				wait = 250 * time.Millisecond
			}
			head := make(chan *Lease, 1)
			go func() {
				head <- p.AcquireRequest(ctx, Request{Wait: wait, Allowed: func(r config.Replica) bool { return r.URL == a.Replica.URL }})
			}()
			waitQueued(t, p, 1)
			var next chan *Lease
			if wake != "new caller" {
				next = make(chan *Lease, 1)
				go func() { next <- p.AcquireRequest(t.Context(), Request{Wait: time.Second}) }()
				waitQueued(t, p, 2)
			}
			if wake == "release" {
				b.Release()
			} else {
				// Simulate capacity becoming usable without a release in this pool
				// (e.g. passive cooldown expiry). Removing a waiter must redispatch.
				p.mu.Lock()
				p.replicas[b.index].inflight--
				b.once.Do(func() {})
				p.mu.Unlock()
			}
			if wake == "cancel" {
				cancel()
			}
			var l *Lease
			if wake == "new caller" {
				l = p.AcquireRequest(t.Context(), Request{})
			} else {
				select {
				case l = <-next:
				case <-time.After(time.Second):
					t.Fatal("free capacity blocked behind unserviceable waiter")
				}
			}
			if l == nil || l.index != b.index {
				t.Fatal("free capacity was not assigned")
			}
			l.Release()
			cancel()
			if l := <-head; l != nil {
				l.Release()
				t.Fatal("cancelled head received a lease")
			}
		})
	}
}

func TestSessionRemovalMovesOnlyRemovedReplicaSessions(t *testing.T) {
	p := fullPool(1)
	for i := range 2 {
		p.replicas = append(p.replicas, replica{config: config.Replica{URL: fmt.Sprintf("http://extra%d", i), Weight: 1, MaxInflight: 1}, healthy: true})
	}
	homes := map[string]string{}
	for i := range 500 {
		session := fmt.Sprintf("session-%d", i)
		l := p.AcquireRequest(t.Context(), Request{Session: session})
		homes[session] = l.Replica.URL
		l.Release()
	}
	removed := p.replicas[1].config.URL
	p.replicas = append(p.replicas[:1], p.replicas[2:]...)
	for session, home := range homes {
		l := p.AcquireRequest(t.Context(), Request{Session: session})
		if home != removed && l.Replica.URL != home {
			t.Fatal("removing another replica moved a session")
		}
		l.Release()
	}
}

func BenchmarkSessionReservation(b *testing.B) {
	for _, size := range []int{32, 1 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			p := fullPool(1)
			r := Request{Session: strings.Repeat("s", size)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				l := p.AcquireRequest(b.Context(), r)
				l.Release()
			}
		})
	}
}

// BenchmarkQueuedChurn measures acquire/release cycles while far more callers
// queue than the replicas admit, as during a burst against busy GPU servers.
func BenchmarkQueuedChurn(b *testing.B) {
	type key struct{}
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{
		{URL: "http://a", Weight: 1, MaxInflight: 8}, {URL: "http://b", Weight: 1, MaxInflight: 8}}}}}
	m := New(cfg, nil)
	p := m.Pools["m"]
	for i := range p.replicas {
		p.replicas[i].healthy = true
	}
	// Like the proxy's filter: a context lookup per eligibility check.
	ctx := context.WithValue(b.Context(), key{}, "principal")
	allowed := func(config.Replica) bool { return ctx.Value(key{}) != nil }
	var remaining atomic.Int64
	remaining.Store(int64(b.N))
	b.ResetTimer()
	var wg sync.WaitGroup
	for range 256 {
		wg.Go(func() {
			for remaining.Add(-1) >= 0 {
				l := p.AcquireRequest(ctx, Request{Wait: time.Minute, Allowed: allowed})
				if l == nil {
					b.Error("queued caller timed out")
					return
				}
				l.Release()
			}
		})
	}
	wg.Wait()
}

// Only the queue head redispatches on a notification. A release in another
// revision must still reach a serviceable caller behind an unserviceable head.
func TestDrainingReleaseServesWaiterBehindUnserviceableHead(t *testing.T) {
	configuration := func(weight float64) *config.Config {
		return &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: "http://a", Weight: weight, MaxInflight: 1}}}}}
	}
	draining := New(configuration(1), nil)
	draining.Pools["m"].replicas[0].healthy = true
	held := draining.Pools["m"].Acquire(nil)
	active := Replace(configuration(2), nil, draining)
	p := active.Pools["m"]
	p.replicas[0].healthy = true
	head := make(chan *Lease, 1)
	go func() {
		head <- p.AcquireRequest(t.Context(), Request{Wait: 2 * time.Second, Allowed: func(config.Replica) bool { return false }})
	}()
	waitQueued(t, p, 1)
	next := make(chan *Lease, 1)
	go func() { next <- p.AcquireRequest(t.Context(), Request{Wait: 2 * time.Second}) }()
	waitQueued(t, p, 2)
	held.Release() // frees shared capacity without a release in p itself
	select {
	case l := <-next:
		if l == nil {
			t.Fatal("serviceable waiter got no lease")
		}
		l.Release()
	case <-time.After(time.Second):
		t.Fatal("capacity freed by a draining revision stayed behind the head")
	}
	if l := <-head; l != nil {
		t.Fatal("unserviceable head received a lease")
	}
	if p.Queued() != 0 {
		t.Fatal("waiters leaked", p.Queued())
	}
}

func TestCapacityEventsArePerModelAndSurviveRemoval(t *testing.T) {
	two := &config.Config{Models: map[string]config.Model{
		"a": {Pool: []config.Replica{{URL: "http://a", Weight: 1, MaxInflight: 1}}},
		"b": {Pool: []config.Replica{{URL: "http://b", Weight: 1, MaxInflight: 1}}}}}
	first := New(two, nil)
	if first.Pools["a"].events == first.Pools["b"].events || first.Pools["a"].events == nil {
		t.Fatal("models share wakeups")
	}
	onlyB := &config.Config{Models: map[string]config.Model{"b": two.Models["b"]}}
	restored := Replace(two, nil, Replace(onlyB, nil, first))
	if restored.Pools["a"] == first.Pools["a"] || restored.Pools["a"].events != first.Pools["a"].events {
		t.Fatal("a restored model must observe releases of its draining revision")
	}
}
