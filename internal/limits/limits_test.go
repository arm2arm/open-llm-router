package limits

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

type loadFunc func(context.Context, string, calendar) (ledger, error)

func (f loadFunc) load(ctx context.Context, user string, c calendar) (ledger, error) {
	return f(ctx, user, c)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
func ptr[T any](v T) *T                  { return &v }
func setup(t *testing.T, zone, instant string, load loadFunc) (*Manager, *testClock) {
	t.Helper()
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: now}
	if load == nil {
		load = func(context.Context, string, calendar) (ledger, error) { return nil, nil }
	}
	return newManager(load, location, clock.Now), clock
}
func acquire(t *testing.T, m *Manager, user string, p policy.Policy) *Lease {
	t.Helper()
	l, err := m.Acquire(t.Context(), user, p)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func snapshot(t *testing.T, m *Manager, user string, p policy.Policy) Snapshot {
	t.Helper()
	s, err := m.Snapshot(t.Context(), user, p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func rejected(t *testing.T, m *Manager, user string, p policy.Policy, gate string, retry int) *Rejection {
	t.Helper()
	l, err := m.Acquire(t.Context(), user, p)
	if l != nil {
		l.Release()
	}
	var r *Rejection
	if !errors.As(err, &r) || r.Limit != gate || r.RetryAfter != retry {
		t.Fatalf("gate=%s: %v, want retry %d", gate, err, retry)
	}
	return r
}

func TestGateOrderAndSharedUserSlots(t *testing.T) {
	m, c := setup(t, "UTC", "2026-05-05T12:00:00Z", nil)
	p := policy.Policy{MaxConcurrent: ptr(2), RPM: ptr(2), TokensPerDay: ptr(int64(10))}
	a, b := acquire(t, m, "user", p), acquire(t, m, "user", p)
	rejected(t, m, "user", p, "max_concurrent", 1)
	// Independent users have their own slots and RPM window.
	acquire(t, m, "other-user", p).Release()
	a.Complete(usage.Record{StartedAt: c.Now(), Model: "chat", InputTokens: 3, OutputTokens: 7})
	a.Release() // completion and cancellation cannot release twice
	rejected(t, m, "user", p, "rpm", 60)
	b.Release()
	c.Add(time.Minute)
	rejected(t, m, "user", p, "tokens_per_day", 12*60*60-60)
	s := snapshot(t, m, "user", p)
	if s.InFlight != 0 || s.Used.Today != 10 || *s.Used.RPM != 1 || *s.Remaining.Today != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestSlidingRPMRetryAndSnapshotDoesNotConsume(t *testing.T) {
	m, c := setup(t, "UTC", "2026-05-05T12:00:00Z", nil)
	p := policy.Policy{RPM: ptr(2)}
	acquire(t, m, "u", p).Release()
	c.Add(10*time.Second + 200*time.Millisecond)
	acquire(t, m, "u", p).Release()
	r := rejected(t, m, "u", p, "rpm", 50)
	if !r.ResetAt.Equal(time.Date(2026, 5, 5, 12, 1, 0, 0, time.UTC)) {
		t.Fatal(r)
	}
	for range 3 {
		if *snapshot(t, m, "u", p).Used.RPM != 2 {
			t.Fatal("snapshot changed RPM")
		}
	}
	c.Add(49*time.Second + 800*time.Millisecond)
	acquire(t, m, "u", p).Release() // exactly 60s expires the oldest attempt
	rejected(t, m, "u", p, "rpm", 11)
}

func TestQuotaCalendarResetOvershootAndMonth(t *testing.T) {
	m, c := setup(t, "Europe/Berlin", "2026-03-28T23:00:00Z", nil)
	p := policy.Policy{TokensPerDay: ptr(int64(10)), TokensPerMonth: ptr(int64(25)), MaxConcurrent: ptr(2)}
	a, b := acquire(t, m, "u", p), acquire(t, m, "u", p)
	a.Complete(usage.Record{StartedAt: c.Now(), Model: "chat", InputTokens: 8, OutputTokens: 8})
	b.Complete(usage.Record{StartedAt: c.Now(), Model: "chat", InputTokens: 2, OutputTokens: 2})
	rejected(t, m, "u", p, "tokens_per_day", 23*3600) // DST spring day is 23 hours
	s := snapshot(t, m, "u", p)
	if s.Used.Today != 20 || s.Used.Month != 20 || *s.Remaining.Today != 0 || *s.Remaining.Month != 5 {
		t.Fatal(s)
	}
	c.Add(23 * time.Hour)
	l := acquire(t, m, "u", p)
	l.Complete(usage.Record{StartedAt: c.Now(), Model: "chat", InputTokens: 3, OutputTokens: 3})
	rejected(t, m, "u", p, "tokens_per_month", 48*3600)
	c.Add(48 * time.Hour)
	s = snapshot(t, m, "u", p)
	if s.Used.Today != 0 || s.Used.Month != 0 || len(s.Models) != 0 {
		t.Fatal(s)
	}
	acquire(t, m, "u", p).Release()
}

func TestLateCompletionStaysInStartDay(t *testing.T) {
	m, c := setup(t, "UTC", "2026-12-31T23:59:59Z", nil)
	p := policy.Policy{TokensPerDay: ptr(int64(5)), TokensPerMonth: ptr(int64(5))}
	l := acquire(t, m, "u", p)
	started := c.Now()
	c.Add(time.Second)
	l.Complete(usage.Record{StartedAt: started, Model: "chat", InputTokens: 10})
	s := snapshot(t, m, "u", p)
	if s.Used.Today != 0 || s.Used.Month != 0 || s.ResetAt.Month.Year() != 2027 || s.ResetAt.Month.Month() != time.February {
		t.Fatal(s)
	}
	acquire(t, m, "u", p).Release()
}

func TestLoadOnceConcurrentAccountingAndAverages(t *testing.T) {
	var loads atomic.Int32
	m, c := setup(t, "UTC", "2026-05-05T12:00:00Z", func(context.Context, string, calendar) (ledger, error) {
		loads.Add(1)
		return ledger{"2026-05-04": {"yesterday": {input: 10, output: 20}}, "2026-05-05": {"chat": {input: 3, output: 7}}}, nil
	})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			l := acquire(t, m, "u", policy.Policy{})
			l.Complete(usage.Record{StartedAt: c.Now(), Model: "chat", InputTokens: 3, OutputTokens: 7, TTFBMS: ptr(int64(50)), LatencyMS: 400, IsStream: i%2 == 0})
			l.Release()
		})
	}
	wg.Wait()
	s := snapshot(t, m, "u", policy.Policy{})
	if loads.Load() != 1 || s.InFlight != 0 || s.Used.Today != 1010 || s.Used.Month != 1040 || len(s.Models) != 1 {
		t.Fatal(s, loads.Load())
	}
	model := s.Models[0]
	if model.InputTokens != 303 || model.OutputTokens != 707 || *model.AverageTTFBMS != 50 || *model.AverageTPS != 20 {
		t.Fatal(model)
	}
	if s.Limits.RPM != nil || s.Remaining.Month != nil || s.Used.RPM != nil || s.ResetAt.RPM != nil {
		t.Fatal("unlimited fields must be null", s)
	}
}

func TestLoadFailureReleasesSlotAndCanRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	m, _ := setup(t, "UTC", "2026-05-05T12:00:00Z", func(context.Context, string, calendar) (ledger, error) {
		if fail.Load() {
			return nil, errors.New("private database error")
		}
		return nil, nil
	})
	p := policy.Policy{MaxConcurrent: ptr(1)}
	if _, err := m.Acquire(t.Context(), "u", p); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	fail.Store(false)
	acquire(t, m, "u", p).Release()
	if snapshot(t, m, "u", p).InFlight != 0 {
		t.Fatal("leaked slot")
	}
}

func TestLoadWaitHonorsCancellationAndDoesNotBlockOtherUsers(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	m, _ := setup(t, "UTC", "2026-05-05T12:00:00Z", func(ctx context.Context, u string, _ calendar) (ledger, error) {
		if u == "blocked" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, nil
	})
	done := make(chan error, 1)
	go func() {
		l, err := m.Acquire(t.Context(), "blocked", policy.Policy{})
		if l != nil {
			l.Release()
		}
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Acquire(ctx, "blocked", policy.Policy{}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	acquire(t, m, "other", policy.Policy{}).Release()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if snapshot(t, m, "blocked", policy.Policy{}).InFlight != 0 {
		t.Fatal("leaked cancelled slot")
	}
}

func TestLoadedQuotaAndSaturatingTotals(t *testing.T) {
	m, c := setup(t, "UTC", "2026-05-05T12:00:00Z", func(context.Context, string, calendar) (ledger, error) {
		return ledger{"2026-05-05": {"chat": {input: math.MaxInt64 - 1}}}, nil
	})
	acquire(t, m, "u", policy.Policy{}).Complete(usage.Record{StartedAt: c.Now(), Model: "chat", InputTokens: 4, OutputTokens: 5})
	s := snapshot(t, m, "u", policy.Policy{TokensPerDay: ptr(int64(100))})
	if s.Used.Today != math.MaxInt64 || *s.Remaining.Today != 0 {
		t.Fatal(s)
	}
	rejected(t, m, "u", policy.Policy{TokensPerMonth: ptr(int64(100))}, "tokens_per_month", 27*86400-12*3600)
}

func TestCalendarAutumnDST(t *testing.T) {
	m, _ := setup(t, "Europe/Berlin", "2026-10-24T22:00:00Z", nil)
	c := calendarAt(m.Now(), m.location)
	if c.nextDay.Sub(c.day) != 25*time.Hour {
		t.Fatal(c)
	}
}
