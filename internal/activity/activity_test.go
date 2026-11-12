package activity

import (
	"testing"
	"time"
)

func TestDeliveryIsolationAndLimits(t *testing.T) {
	done := make(chan struct{})
	b := New(done, nil, nil)
	if b.Watching("alice") {
		t.Fatal("nobody watches yet")
	}
	a, ok := b.Subscribe("alice")
	if !ok || !b.Watching("alice") || b.Watching("bob") {
		t.Fatal("subscription state")
	}
	bob, _ := b.Subscribe("bob")
	b.Publish("alice", Event{Type: "started", RequestID: "r1"})
	select {
	case e := <-a.C:
		if e.RequestID != "r1" {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("not delivered")
	}
	select {
	case e := <-bob.C:
		t.Fatal("cross-user delivery", e)
	default:
	}
	for range MaxPerUser - 1 {
		if _, ok := b.Subscribe("alice"); !ok {
			t.Fatal("refused below the limit")
		}
	}
	if _, ok := b.Subscribe("alice"); ok {
		t.Fatal("accepted above the limit")
	}
	b.Unsubscribe(a)
	if _, ok := b.Subscribe("alice"); !ok {
		t.Fatal("unsubscribe did not free a slot")
	}
	b.Unsubscribe(bob)
	b.Unsubscribe(bob) // idempotent
	if b.Watching("bob") {
		t.Fatal("still watching")
	}
	close(done)
	select {
	case <-b.Done():
	default:
		t.Fatal("done not propagated")
	}
}

func TestPublishNeverBlocks(t *testing.T) {
	b := New(make(chan struct{}), nil, nil)
	s, _ := b.Subscribe("u")
	start := time.Now()
	for range Buffer * 4 {
		b.Publish("u", Event{Type: "progress"})
	}
	if time.Since(start) > time.Second || len(s.C) != Buffer || b.Dropped.Load() != Buffer*3 {
		t.Fatalf("blocked or lost count: %d %d", len(s.C), b.Dropped.Load())
	}
	var nilBroker *Broker
	nilBroker.Publish("u", Event{})
	if nilBroker.Watching("u") {
		t.Fatal("nil broker")
	}
}

func BenchmarkPublishUnwatched(b *testing.B) {
	broker := New(make(chan struct{}), nil, nil)
	for b.Loop() {
		broker.Publish("user", Event{Type: "progress", RequestID: "r"})
	}
}
