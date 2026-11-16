package pool

import (
	"context"
	"fmt"
	"strings"
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
