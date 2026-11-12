package pool

import (
	"net/http"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestRevisionsPreserveCapacityAcrossEditsRemovalAndRollback(t *testing.T) {
	configuration := func(url string, weight float64) *config.Config {
		return &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: url, Weight: weight, MaxInflight: 1}}}}}
	}
	initial := New(configuration("http://a", 1), http.DefaultTransport)
	initial.Pools["m"].replicas[0].healthy = true
	lease := initial.Pools["m"].Acquire(nil)
	if lease == nil {
		t.Fatal("no initial lease")
	}
	defer lease.Release()
	unchanged := Replace(configuration("http://a", 1), http.DefaultTransport, initial)
	if unchanged.Pools["m"] != initial.Pools["m"] {
		t.Fatal("unchanged pool was rebuilt")
	}
	changed := Replace(configuration("http://a", 2), http.DefaultTransport, initial)
	changed.Pools["m"].replicas[0].healthy = true
	if changed.Pools["m"].Acquire(nil) != nil || changed.Pools["m"].Snapshot()[0].Inflight != 1 {
		t.Fatal("edit reset an outstanding replica reservation")
	}
	removed := Replace(configuration("http://b", 1), http.DefaultTransport, changed)
	rollback := Replace(configuration("http://a", 1), http.DefaultTransport, removed)
	rollback.Pools["m"].replicas[0].healthy = true
	if rollback.Pools["m"].Acquire(nil) != nil {
		t.Fatal("rollback bypassed the draining reservation")
	}
	acquired := make(chan *Lease, 1)
	go func() {
		acquired <- rollback.Pools["m"].AcquireRequest(t.Context(), Request{Wait: time.Second})
	}()
	deadline := time.Now().Add(time.Second)
	for rollback.Pools["m"].Queued() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rollback.Pools["m"].Queued() != 1 {
		t.Fatal("request did not queue")
	}
	lease.Release()
	select {
	case next := <-acquired:
		if next == nil {
			t.Fatal("release in a draining revision did not wake the active queue")
		}
		next.Release()
	case <-time.After(time.Second):
		t.Fatal("active queue was not notified")
	}
	if next := rollback.Pools["m"].Acquire(nil); next == nil {
		t.Fatal("released capacity did not become available")
	} else {
		next.Release()
	}
}
