// Package limits enforces per-user limits for a single router instance.
package limits

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnavailable = errors.New("quota counters are temporarily unavailable")

// Rejection identifies the failed gate and its next retry time. For concurrency
// the time is an estimate: another call may release a slot sooner.
type Rejection struct {
	Limit      string
	ResetAt    time.Time
	RetryAfter int
}

func (r *Rejection) Error() string { return r.Limit + " exceeded" }

func reject(limit string, now, reset time.Time) *Rejection {
	return &Rejection{Limit: limit, ResetAt: reset, RetryAfter: max(1, int(math.Ceil(reset.Sub(now).Seconds())))}
}

type modelTotals struct {
	input, output        int64
	ttfbSum, rateSum     float64
	ttfbCount, rateCount int64
}

type day map[string]*modelTotals
type ledger map[string]day

type store interface {
	load(context.Context, string, calendar) (ledger, error)
}

type userState struct {
	mu       sync.Mutex
	user     string
	inflight [2]int // by class: interactive, batch
	requests []time.Time
	loaded   bool
	loading  chan struct{}
	days     ledger
	// tpm token bucket: level tokens at tpmAt, refilled at tpm/60 per second
	// up to tpm. Completed calls subtract their tokens; the level can go
	// negative, and admission needs at least one token.
	tpmLevel float64
	tpmAt    time.Time
	tpmSet   bool
	reserved int64 // strict-quota reservations of calls in flight
}

// Key classes.
const (
	ClassInteractive = "interactive"
	ClassBatch       = "batch"
)

func classIndex(class string) int {
	if class == ClassBatch {
		return 1
	}
	return 0
}

func (s *userState) inFlight() int { return s.inflight[0] + s.inflight[1] }

// refillTPM brings the bucket to now for a capacity of tpm tokens per minute.
func (s *userState) refillTPM(tpm int64, now time.Time) {
	capacity := float64(tpm)
	if !s.tpmSet {
		s.tpmLevel, s.tpmAt, s.tpmSet = capacity, now, true
		return
	}
	if elapsed := now.Sub(s.tpmAt).Seconds(); elapsed > 0 {
		s.tpmLevel += elapsed * capacity / 60
	}
	s.tpmLevel = min(s.tpmLevel, capacity)
	s.tpmAt = now
}

// Admission describes one call: the key's class and, for strict quotas, the
// tokens to reserve until the call completes.
type Admission struct {
	User    string
	Class   string
	Reserve int64
}

type Manager struct {
	mu       sync.Mutex
	users    map[string]*userState
	store    store
	location *time.Location
	clock    func() time.Time
}

// New loads quotas lazily from usage_daily. Use the same location for the usage
// writer. The clock is injectable so calendar boundaries can be tested.
func New(pool *pgxpool.Pool, location *time.Location, clock func() time.Time) *Manager {
	return newManager(postgresStore{pool}, location, clock)
}

func newManager(store store, location *time.Location, clock func() time.Time) *Manager {
	return &Manager{users: make(map[string]*userState), store: store, location: location, clock: clock}
}

func (m *Manager) Now() time.Time { return m.clock() }

func (m *Manager) user(id string) *userState {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.users[id]
	if s == nil {
		s = &userState{user: id}
		m.users[id] = s
	}
	return s
}

func pruneRPM(s *userState, now time.Time) {
	first := 0
	for first < len(s.requests) && !s.requests[first].After(now.Add(-time.Minute)) {
		first++
	}
	if first > 0 {
		copy(s.requests, s.requests[first:])
		s.requests = s.requests[:len(s.requests)-first]
	}
}

// Acquire admits an interactive call without a reservation.
func (m *Manager) Acquire(ctx context.Context, user string, p policy.Policy) (*Lease, error) {
	return m.AcquireFor(ctx, Admission{User: user}, p)
}

// AcquireFor applies concurrency for the key's class, sliding-minute RPM, the
// TPM bucket, then remaining token quota. A request that passes RPM consumes
// that attempt even if a later gate rejects it. A concurrency rejection
// consumes no RPM. With a strict quota, the reservation must fit into the
// remaining quota next to the reservations of calls already in flight.
func (m *Manager) AcquireFor(ctx context.Context, a Admission, p policy.Policy) (*Lease, error) {
	s := m.user(a.User)
	class := classIndex(a.Class)
	gate := "max_concurrent"
	if class == 1 {
		gate = "max_concurrent_batch"
	}
	s.mu.Lock()
	now := m.Now()
	if limit := p.Concurrency(a.Class); limit != nil && s.inflight[class] >= *limit {
		s.mu.Unlock()
		return nil, reject(gate, now, now.Add(time.Second))
	}
	s.inflight[class]++
	lease := &Lease{manager: m, state: s, class: class}
	pruneRPM(s, now)
	if p.RPM != nil {
		if len(s.requests) >= *p.RPM {
			reset := s.requests[0].Add(time.Minute)
			s.inflight[class]--
			s.mu.Unlock()
			return nil, reject("rpm", now, reset)
		}
		s.requests = append(s.requests, now)
	}
	if p.TPM != nil {
		s.refillTPM(*p.TPM, now)
		if s.tpmLevel < 1 {
			wait := time.Duration((1 - s.tpmLevel) * 60 / float64(*p.TPM) * float64(time.Second))
			s.inflight[class]--
			s.mu.Unlock()
			return nil, reject("tpm", now, now.Add(wait))
		}
	}
	s.mu.Unlock()
	if err := m.ensureLoaded(ctx, s, now); err != nil {
		lease.Release()
		return nil, err
	}
	s.mu.Lock()
	now = m.Now()
	period := calendarAt(now, m.location)
	pruneDays(s, period)
	today, month := used(s.days, period)
	var err error
	reserve := int64(0)
	if p.StrictQuota {
		reserve = max(a.Reserve, 0)
	}
	exceeds := func(limit *int64, used int64) bool {
		if limit == nil {
			return false
		}
		if used >= *limit {
			return true
		}
		return p.StrictQuota && add(add(used, s.reserved), reserve) > *limit
	}
	if exceeds(p.TokensPerDay, today) {
		err = reject("tokens_per_day", now, period.nextDay)
	} else if exceeds(p.TokensPerMonth, month) {
		err = reject("tokens_per_month", now, period.nextMonth)
	} else {
		s.reserved = add(s.reserved, reserve)
		lease.reserved = reserve
	}
	s.mu.Unlock()
	if err != nil {
		lease.Release()
		return nil, err
	}
	return lease, nil
}

func (m *Manager) ensureLoaded(ctx context.Context, s *userState, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		s.mu.Lock()
		if s.loaded {
			s.mu.Unlock()
			return nil
		}
		if s.loading != nil {
			ready := s.loading
			s.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return ErrUnavailable
			}
		}
		s.loading = make(chan struct{})
		ready := s.loading
		s.mu.Unlock()
		days, err := m.store.load(ctx, s.user, calendarAt(now, m.location))
		s.mu.Lock()
		if err == nil {
			if days == nil {
				days = make(ledger)
			}
			s.days, s.loaded = days, true
		}
		s.loading = nil
		close(ready)
		s.mu.Unlock()
		if err != nil {
			return ErrUnavailable
		}
		return nil
	}
}

// Lease owns one concurrency slot. Complete accounts tokens before releasing
// it; Release handles calls such as count_tokens that have no usage record.
// Both are idempotent and safe against concurrent completion/cancellation.
type Lease struct {
	once     sync.Once
	manager  *Manager
	state    *userState
	class    int
	reserved int64
}

func (l *Lease) Release()                     { l.finish(nil) }
func (l *Lease) Complete(record usage.Record) { l.finish(&record) }

func (l *Lease) finish(record *usage.Record) {
	l.once.Do(func() {
		s := l.state
		s.mu.Lock()
		defer s.mu.Unlock()
		if record != nil {
			date := record.StartedAt.In(l.manager.location).Format("2006-01-02")
			if s.days[date] == nil {
				s.days[date] = make(day)
			}
			if s.days[date][record.Model] == nil {
				s.days[date][record.Model] = &modelTotals{}
			}
			totals := s.days[date][record.Model]
			totals.input = add(totals.input, record.InputTokens)
			totals.output = add(totals.output, record.OutputTokens)
			if record.TTFBMS != nil {
				totals.ttfbSum += float64(*record.TTFBMS)
				totals.ttfbCount++
			}
			if record.IsStream && record.OutputTokens > 0 && record.TTFBMS != nil && record.LatencyMS > *record.TTFBMS {
				totals.rateSum += record.TokensPerSecond()
				totals.rateCount++
			}
			if s.tpmSet {
				s.tpmLevel -= float64(add(max(record.InputTokens, 0), record.OutputTokens))
			}
		}
		s.reserved -= l.reserved
		s.inflight[l.class]--
	})
}

func add(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + max(b, 0)
}

type calendar struct{ day, nextDay, month, nextMonth time.Time }

func calendarAt(now time.Time, location *time.Location) calendar {
	now = now.In(location)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	return calendar{day: day, nextDay: day.AddDate(0, 0, 1), month: month, nextMonth: month.AddDate(0, 1, 0)}
}

func used(days ledger, period calendar) (today, month int64) {
	date, prefix := period.day.Format("2006-01-02"), period.month.Format("2006-01")
	for d, models := range days {
		if !strings.HasPrefix(d, prefix) {
			continue
		}
		for _, model := range models {
			tokens := add(model.input, model.output)
			month = add(month, tokens)
			if d == date {
				today = add(today, tokens)
			}
		}
	}
	return today, month
}

func pruneDays(s *userState, period calendar) {
	for date := range s.days {
		if date < period.month.Format("2006-01-02") {
			delete(s.days, date)
		}
	}
}

type EffectiveLimits struct {
	TokensPerDay       *int64 `json:"tokens_per_day"`
	TokensPerMonth     *int64 `json:"tokens_per_month"`
	RPM                *int   `json:"rpm"`
	TPM                *int64 `json:"tpm"`
	MaxConcurrent      *int   `json:"max_concurrent"`
	MaxConcurrentBatch *int   `json:"max_concurrent_batch"`
	StrictQuota        bool   `json:"strict_quota"`
}

// Effective lists a policy's limits for the usage APIs.
func Effective(p policy.Policy) EffectiveLimits {
	return EffectiveLimits{TokensPerDay: p.TokensPerDay, TokensPerMonth: p.TokensPerMonth, RPM: p.RPM, TPM: p.TPM,
		MaxConcurrent: p.MaxConcurrent, MaxConcurrentBatch: p.MaxConcurrentBatch, StrictQuota: p.StrictQuota}
}

type Used struct {
	Today int64 `json:"tokens_today"`
	Month int64 `json:"tokens_this_month"`
	RPM   *int  `json:"rpm"`
}

type Remaining struct {
	Today *int64 `json:"tokens_today"`
	Month *int64 `json:"tokens_this_month"`
	RPM   *int   `json:"rpm"`
	TPM   *int64 `json:"tpm"`
}

type Resets struct {
	Day   time.Time  `json:"day"`
	Month time.Time  `json:"month"`
	RPM   *time.Time `json:"rpm"`
}

type ModelUsage struct {
	Model         string   `json:"model"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	AverageTTFBMS *float64 `json:"average_ttfb_ms"`
	AverageTPS    *float64 `json:"average_tokens_per_second"`
}

type Snapshot struct {
	Timezone  string          `json:"timezone"`
	Limits    EffectiveLimits `json:"limits"`
	Used      Used            `json:"used"`
	Remaining Remaining       `json:"remaining"`
	ResetAt   Resets          `json:"reset_at"`
	InFlight  int             `json:"in_flight"`
	Batch     int             `json:"in_flight_batch"`
	Reserved  int64           `json:"reserved_tokens"`
	Models    []ModelUsage    `json:"models"`
}

func remaining(limit *int64, used int64) *int64 {
	if limit == nil {
		return nil
	}
	value := max(0, *limit-used)
	return &value
}

func (m *Manager) Snapshot(ctx context.Context, user string, p policy.Policy) (Snapshot, error) {
	s := m.user(user)
	if err := m.ensureLoaded(ctx, s, m.Now()); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := m.Now()
	period := calendarAt(now, m.location)
	today, month := used(s.days, period)
	pruneRPM(s, now)
	result := Snapshot{
		Timezone: m.location.String(),
		Limits:   Effective(p),
		Used:     Used{Today: today, Month: month}, Remaining: Remaining{Today: remaining(p.TokensPerDay, today), Month: remaining(p.TokensPerMonth, month)},
		ResetAt: Resets{Day: period.nextDay, Month: period.nextMonth}, InFlight: s.inFlight(), Batch: s.inflight[1],
		Reserved: s.reserved, Models: make([]ModelUsage, 0),
	}
	if p.TPM != nil {
		s.refillTPM(*p.TPM, now)
		left := max(0, int64(s.tpmLevel))
		result.Remaining.TPM = &left
	}
	if p.RPM != nil {
		used, left := len(s.requests), max(0, *p.RPM-len(s.requests))
		result.Used.RPM, result.Remaining.RPM = &used, &left
		if len(s.requests) > 0 {
			reset := s.requests[0].Add(time.Minute)
			result.ResetAt.RPM = &reset
		}
	}
	for name, totals := range s.days[period.day.Format("2006-01-02")] {
		if totals.input == 0 && totals.output == 0 && totals.ttfbCount == 0 {
			continue
		}
		model := ModelUsage{Model: name, InputTokens: totals.input, OutputTokens: totals.output}
		if totals.ttfbCount > 0 {
			avg := totals.ttfbSum / float64(totals.ttfbCount)
			model.AverageTTFBMS = &avg
		}
		if totals.rateCount > 0 {
			avg := totals.rateSum / float64(totals.rateCount)
			model.AverageTPS = &avg
		}
		result.Models = append(result.Models, model)
	}
	slices.SortFunc(result.Models, func(a, b ModelUsage) int { return strings.Compare(a.Model, b.Model) })
	// Past months cannot affect the current windows; don't retain their stats.
	pruneDays(s, period)
	return result, nil
}
