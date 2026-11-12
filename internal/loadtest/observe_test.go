package loadtest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type observationTransport func(*http.Request) (*http.Response, error)

func (f observationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func observationResponse(goroutines, inFlight int) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`
# TYPE process_cpu_seconds_total counter
process_cpu_seconds_total 1
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 1024
# TYPE go_memstats_heap_inuse_bytes gauge
go_memstats_heap_inuse_bytes 1024
# TYPE go_goroutines gauge
go_goroutines %d
# TYPE olr_replica_in_flight gauge
olr_replica_in_flight %d
# TYPE olr_usage_write_failures_total counter
olr_usage_write_failures_total 0
# TYPE olr_usage_dropped_total counter
olr_usage_dropped_total 0
`, goroutines, inFlight)))}
}

func usageResponse() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"in_flight":0}`))}
}

func TestRuntimeObservationsDoNotOverlap(t *testing.T) {
	var active atomic.Int64
	var overlap atomic.Bool
	r := runner{o: Options{Router: "http://router"}, probes: &http.Client{Transport: observationTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/metrics" {
			return usageResponse(), nil
		}
		if active.Add(1) > 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		time.Sleep(time.Millisecond)
		return observationResponse(400, 0), nil
	})}}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := r.snapshot(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if overlap.Load() {
		t.Fatal("peak monitor and idle sampler overlapped their metrics requests")
	}
}

func TestIdleGoroutineMinimumRequiresDrainedWindowAndRetainsLeaks(t *testing.T) {
	observations := 0
	leaked := false
	r := runner{o: Options{Router: "http://router", Settle: 150 * time.Millisecond}, load: &http.Client{}, probes: &http.Client{Transport: observationTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/metrics" {
			return usageResponse(), nil
		}
		if leaked {
			return observationResponse(500, 0), nil
		}
		observations++
		switch observations {
		case 1:
			return observationResponse(3, 1), nil // Not drained: exclude this sample.
		case 2:
			return observationResponse(400, 0), nil
		default:
			return observationResponse(406, 0), nil // Transient scrape/health work.
		}
	})}}
	s, err := r.idle(context.Background())
	if err != nil || s.Goroutines != 406 || s.IdleGoroutinesMin != 400 {
		t.Fatalf("quiet window: %+v, %v", s, err)
	}
	leaked = true
	s, err = r.idle(context.Background())
	if err != nil || s.IdleGoroutinesMin != 500 {
		t.Fatalf("persistent increase was hidden by an earlier window: %+v, %v", s, err)
	}
}
