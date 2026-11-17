// Package requesttrace captures personal LLM content separately from accounting.
// The forwarding path only copies bounded bytes: decoding and SQL run in workers.
package requesttrace

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arm2arm/open-llm-router/internal/activity"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxInput bounds the bytes copied from the request. Binary payloads are
	// removed in the worker; MaxInputText then bounds the normalized text.
	MaxInput         = 1 << 20
	MaxInputText     = 384 << 10
	MaxRaw           = 128 << 10
	MaxFrames        = 200
	MaxFrameRaw      = 64 << 10
	MaxOutput        = 256 << 10
	MaxChunks        = 2048
	MaxEvents        = 1000
	MaxParameters    = 32 << 10
	MaxProviderUsage = 64 << 10
	// Stored JSON content and the number of retained traces are bounded per
	// account across replicas. Existing content expires normally after upgrade.
	MaxStoredBytes        = 64 << 20
	MaxStoredTraces       = 512
	UserSlots             = 4
	UserCapturesPerMinute = 30
	maxUserBudgets        = 4096
	// Slots cover active captures AND the background queue, not just queued work.
	Slots = 32
)

type Preferences struct {
	Available     bool       `json:"available"`
	Enabled       bool       `json:"enabled"`
	RetentionDays int        `json:"retention_days"`
	UpdatedAt     *time.Time `json:"updated_at"`
	Generation    int64      `json:"-"`
}

func defaults() Preferences { return Preferences{Enabled: false, RetentionDays: 7} }

type Service struct {
	gate             sync.RWMutex
	enabled          bool
	epoch            uint64
	redactor         *redactor
	Written          atomic.Uint64
	Deleted          atomic.Uint64
	RetentionBacklog atomic.Int64
	pool             *pgxpool.Pool
	log              *slog.Logger
	mu               sync.Mutex
	prefs            atomic.Pointer[map[string]Preferences]
	slots            chan struct{}
	budgetMu         sync.Mutex
	budgets          map[string]captureBudget
	now              func() time.Time
	queue            chan *Capture
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	// Activity, when set, announces stored traces to the user's open portal.
	Activity  *activity.Broker
	Processed atomic.Uint64
	Dropped   atomic.Uint64
	Failed    atomic.Uint64
}

func New(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, options ...config.ContentTracing) (*Service, error) {
	s := &Service{pool: pool, log: log, slots: make(chan struct{}, Slots), queue: make(chan *Capture, Slots)}
	var policy config.ContentTracing
	if len(options) > 0 {
		policy = options[0]
	}
	s.enabled = policy.Enabled
	var err error
	s.redactor, err = newRedactor(policy.RedactPatterns)
	if err != nil {
		return nil, err
	}
	if err := s.refresh(ctx); err != nil {
		return nil, errors.New("load request tracing preferences failed")
	}
	run, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for range 2 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case c := <-s.queue:
					ctx, cancel := context.WithTimeout(run, 2*time.Second)
					err := s.persist(ctx, c)
					cancel()
					s.release(c.user)
					s.Processed.Add(1)
					if err != nil && run.Err() == nil {
						s.Failed.Add(1)
					}
				case <-run.Done():
					return
				}
			}
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		var lastDropped, lastFailed uint64
		for {
			select {
			case <-ticker.C:
				dropped, failed := s.Dropped.Load(), s.Failed.Load()
				if dropped != lastDropped || failed != lastFailed {
					s.log.Warn("request traces skipped", "dropped", dropped, "failed", failed)
					lastDropped, lastFailed = dropped, failed
				}
				ctx, cancel := context.WithTimeout(run, 5*time.Second)
				_ = s.refresh(ctx)
				_ = s.Prune(ctx)
				cancel()
			case <-run.Done():
				return
			}
		}
	}()
	return s, nil
}

// Close is called after the proxy has drained. Trace loss on shutdown never
// prevents accounting flush or prolongs shutdown for a database outage.
func (s *Service) Close() {
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for len(s.slots) > 0 {
		select {
		case <-deadline.C:
			s.cancel()
			s.wg.Wait()
			return
		case <-tick.C:
		}
	}
	s.cancel()
	s.wg.Wait()
}

func (s *Service) refresh(ctx context.Context) error {
	// Serialize refresh with local settings changes, so an older snapshot cannot
	// overwrite the just-saved opt-out. Other instances are gated again in SQL.
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.pool.Query(ctx, `SELECT id::text, trace_enabled, trace_retention_days, trace_generation FROM users WHERE trace_generation<>0 OR trace_enabled OR trace_retention_days<>7`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := make(map[string]Preferences)
	for rows.Next() {
		var id string
		var p Preferences
		if err := rows.Scan(&id, &p.Enabled, &p.RetentionDays, &p.Generation); err != nil {
			return err
		}
		next[id] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.prefs.Store(&next)
	return nil
}

func (s *Service) Preferences(ctx context.Context, user string) (Preferences, error) {
	var p Preferences
	err := s.pool.QueryRow(ctx, `SELECT trace_enabled, trace_retention_days, trace_generation, trace_preferences_set_at FROM users WHERE id=$1`, user).Scan(&p.Enabled, &p.RetentionDays, &p.Generation, &p.UpdatedAt)
	p.Available = s.Enabled()
	return p, err
}

// Save also invalidates queued/in-flight captures. Shortening retention applies
// to existing traces; extending it never resurrects or extends older content.
func (s *Service) Save(ctx context.Context, user string, p Preferences) error {
	if p.RetentionDays < 1 || p.RetentionDays > 30 {
		return errors.New("retention must be between 1 and 30 days")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old Preferences
		if err := tx.QueryRow(ctx, `SELECT trace_enabled,trace_retention_days,trace_generation,trace_preferences_set_at FROM users WHERE id=$1 FOR UPDATE`, user).Scan(&old.Enabled, &old.RetentionDays, &old.Generation, &old.UpdatedAt); err != nil {
			return err
		}
		if old.Enabled == p.Enabled && old.RetentionDays == p.RetentionDays && old.UpdatedAt != nil {
			p.Generation, p.UpdatedAt = old.Generation, old.UpdatedAt
			return nil
		}
		if err := tx.QueryRow(ctx, `UPDATE users SET trace_enabled=$2, trace_retention_days=$3, trace_generation=trace_generation+1,trace_preferences_set_at=clock_timestamp() WHERE id=$1 RETURNING trace_generation,trace_preferences_set_at`, user, p.Enabled, p.RetentionDays).Scan(&p.Generation, &p.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE request_traces SET expires_at=LEAST(expires_at,started_at+make_interval(days=>$2)) WHERE user_id=$1 AND expires_at>started_at+make_interval(days=>$2)`, user, p.RetentionDays); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Action: "trace.preferences", Actor: audit.Actor{Type: "portal_user", ID: user}, TargetType: "user", TargetID: user, Result: audit.Succeeded, Details: map[string]any{"enabled": p.Enabled, "retention_days": p.RetentionDays, "updated_at": p.UpdatedAt}})
	})
	if err == nil {
		next := make(map[string]Preferences)
		if old := s.prefs.Load(); old != nil {
			for id, pref := range *old {
				next[id] = pref
			}
		}
		next[user] = p
		s.prefs.Store(&next)
	}
	return err
}

type chunk struct {
	End int
	MS  int64
}
type Capture struct {
	user               string
	epoch              uint64
	service            *Service
	generation         int64
	operation          string
	session            string
	routing            Routing
	input, output      []byte
	providerUsage      [][]byte
	providerUsageBytes int
	chunks             []chunk
	truncated          bool
	outputStopped      bool
	jsonResponse       bool
	began              time.Time
	record             usage.Record
}

// Begin never performs network I/O or waits for capacity. Unknown (new) users
// inherit disabled/7 days; the writer rechecks the database before storage.
func (s *Service) Begin(user, operation string, began time.Time) *Capture {
	if s == nil || user == "" {
		return nil
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if !s.enabled {
		return nil
	}
	p := defaults()
	ok := false
	if prefs := s.prefs.Load(); prefs != nil {
		p, ok = (*prefs)[user]
	}
	if !ok {
		p = defaults()
	}
	if !p.Enabled {
		return nil
	}
	s.budgetMu.Lock()
	defer s.budgetMu.Unlock()
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if s.budgets == nil {
		s.budgets = make(map[string]captureBudget)
	}
	b, exists := s.budgets[user]
	if !exists && len(s.budgets) >= maxUserBudgets {
		for id, old := range s.budgets {
			if old.active == 0 && !now.Before(old.until) {
				delete(s.budgets, id)
			}
		}
		if len(s.budgets) >= maxUserBudgets {
			s.Dropped.Add(1)
			return nil
		}
	}
	if !now.Before(b.until) {
		b.until, b.used = now.Add(time.Minute), 0
	}
	if b.active >= UserSlots || b.used >= UserCapturesPerMinute {
		s.Dropped.Add(1)
		return nil
	}
	select {
	case s.slots <- struct{}{}:
		b.active++
		b.used++
		s.budgets[user] = b
		return &Capture{user: user, service: s, epoch: s.epoch, generation: p.Generation, operation: operation, began: began}
	default:
		s.Dropped.Add(1)
		return nil
	}
}

type captureBudget struct {
	until        time.Time
	used, active int
}

func (s *Service) release(user string) {
	<-s.slots
	s.budgetMu.Lock()
	b := s.budgets[user]
	b.active--
	s.budgets[user] = b
	s.budgetMu.Unlock()
}

func (c *Capture) Input(p []byte) {
	if c == nil {
		return
	}
	if c.service != nil {
		c.service.gate.RLock()
		defer c.service.gate.RUnlock()
		if !c.service.enabled || c.epoch != c.service.epoch {
			return
		}
	}
	// Never decode partial JSON or accidentally store a cut base64 attachment.
	if len(p) > MaxInput {
		c.truncated = true
		return
	}
	c.input = append([]byte(nil), p...)
}

// Session records a client-provided X-Session-Id for conversation grouping.
// Only its keyed hash is stored.
func (c *Capture) Session(id string) {
	if c != nil {
		c.session, _ = cut(id, 256)
	}
}

// Route records how the router resolved the requested model.
func (c *Capture) Route(requested, canonical, upstream string) {
	if c != nil {
		c.routing.RequestedModel, c.routing.Model, c.routing.UpstreamModel = requested, canonical, upstream
	}
}

// Change records one router modification of the body forwarded upstream.
func (c *Capture) Change(field, action string, from, to any) {
	if c != nil {
		c.routing.Changes = append(c.routing.Changes, Change{Field: field, Action: action, From: from, To: to})
	}
}

// Admission records time spent waiting for quota/concurrency admission.
func (c *Capture) Admission(d time.Duration) {
	if c != nil {
		c.routing.AdmissionMS = d.Milliseconds()
	}
}

// Attempt records one replica selection and upstream exchange. Replica is a
// pool position, never a URL: backend addresses stay operator-only.
func (c *Capture) Attempt(a Attempt) {
	if c != nil && len(c.routing.Attempts) < 4 {
		c.routing.Attempts = append(c.routing.Attempts, a)
	}
}

// Headers records the names, never values, of headers forwarded upstream.
func (c *Capture) Headers(h map[string][]string) {
	if c == nil || c.routing.Headers != nil {
		return
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	c.routing.Headers = names
}

// Upstream records the provider's response status and media type.
func (c *Capture) Upstream(status int, contentType string) {
	if c != nil {
		c.routing.UpstreamStatus = status
		c.routing.ContentType, _ = cut(contentType, 128)
	}
}

func (c *Capture) ResponseJSON(yes bool) {
	if c != nil {
		c.jsonResponse = yes
	}
}
func (c *Capture) Write(p []byte) {
	if c == nil {
		return
	}
	if c.service != nil {
		c.service.gate.RLock()
		defer c.service.gate.RUnlock()
		if !c.service.enabled || c.epoch != c.service.epoch {
			return
		}
	}
	if c.outputStopped {
		return
	}
	if len(c.output)+len(p) > MaxOutput || len(c.chunks) >= MaxChunks {
		c.truncated = true
		c.outputStopped = true
		return
	}
	// Once a chunk is dropped, don't splice later bytes onto the partial JSON.

	c.output = append(c.output, p...)
	c.chunks = append(c.chunks, chunk{len(c.output), time.Since(c.began).Milliseconds()})
}

// Usage copies bounded upstream metadata independently of the client-visible
// output prefix. Numeric decoding happens only in the background worker.
func (c *Capture) Usage(raw []byte) {
	if c == nil || len(raw) == 0 || len(raw) > MaxProviderUsage {
		return
	}
	if c.service != nil {
		c.service.gate.RLock()
		defer c.service.gate.RUnlock()
		if !c.service.enabled || c.epoch != c.service.epoch {
			return
		}
	}
	// Keep recent snapshots, including Anthropic's separate input/output usage.
	for len(c.providerUsage) > 0 && (len(c.providerUsage) >= 4 || c.providerUsageBytes+len(raw) > MaxProviderUsage) {
		c.providerUsageBytes -= len(c.providerUsage[0])
		c.providerUsage[0] = nil
		c.providerUsage = c.providerUsage[1:]
	}
	c.providerUsage = append(c.providerUsage, append([]byte(nil), raw...))
	c.providerUsageBytes += len(raw)
}

func (c *Capture) Finish(r usage.Record) {
	if c == nil {
		return
	}
	c.record = r
	// One slot was reserved at Begin, hence the queue always has room. This is
	// still nonblocking in case a future caller accidentally changes that rule.
	select {
	case c.service.queue <- c:
	default:
		c.service.Dropped.Add(1)
		c.service.release(c.user)
	}
}

func (s *Service) persist(ctx context.Context, c *Capture) error {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if !s.enabled || c.epoch != s.epoch {
		return nil
	}
	detail := c.decode()
	data, err := s.redactor.marshal(detail)
	if err != nil {
		return err
	}
	var sanitized struct {
		Summary json.RawMessage `json:"summary"`
	}
	if err := decodeJSON(data, &sanitized); err != nil {
		return err
	}
	summary := sanitized.Summary
	var conversation *string
	if key := c.conversationKey(detail); key != "" {
		conversation = &key
	}
	// Lock the account before the budget query. A separate statement after the
	// lock obtains a fresh READ COMMITTED snapshot, including a writer that
	// completed while this one waited. Other replicas cannot overspend the cap.
	var stored bool
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var retention int
		err := tx.QueryRow(ctx, `SELECT trace_retention_days FROM users WHERE id=$1 AND trace_enabled AND trace_generation=$2 FOR UPDATE`, c.record.UserID, c.generation).Scan(&retention)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// stored_bytes is generated from PostgreSQL's JSON representation. Use
		// that same representation for this row, including its fixed overhead.
		tag, err := tx.Exec(ctx, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail,summary,conversation_key)
		SELECT $1,$2,$3,$3::timestamptz+make_interval(days=>$8),$4::jsonb,$6::jsonb,$7
		WHERE $3::timestamptz+make_interval(days=>$8)>now()
		AND (SELECT count(*)<$9 AND COALESCE(sum(stored_bytes),0)+octet_length($4::jsonb::text)::bigint+octet_length($6::jsonb::text)::bigint+256<=$5 FROM request_traces WHERE user_id=$2)
		ON CONFLICT(request_id) DO NOTHING`, c.record.RequestID, c.record.UserID, c.record.StartedAt, data, MaxStoredBytes, summary, conversation, retention, MaxStoredTraces)
		stored = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err == nil {
		if stored {
			s.Written.Add(1)
			s.Activity.Publish(c.record.UserID, activity.Event{Type: "trace_ready", RequestID: c.record.RequestID})
		} else {
			s.Dropped.Add(1)
		}
	}
	return err
}

func (s *Service) Prune(ctx context.Context) error {
	// Each transaction is bounded. Access checks exclude expired content even
	// before physical cleanup; a backlog drains over successive ticks.
	tag, err := s.pool.Exec(ctx, `DELETE FROM request_traces WHERE request_id IN (SELECT request_id FROM request_traces WHERE expires_at<=now() LIMIT 1000)`)
	if err != nil {
		s.Failed.Add(1)
		return err
	}
	s.Deleted.Add(uint64(tag.RowsAffected()))
	var backlog int64
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM request_traces WHERE expires_at<=now()`).Scan(&backlog)
	if err == nil {
		s.RetentionBacklog.Store(backlog)
	} else {
		s.Failed.Add(1)
	}
	return err
}

// Erase serializes with writers without changing a setting saved in another tab.
func (s *Service) Erase(ctx context.Context, user string, activity bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p Preferences
	var count int64
	var traceDeleted uint64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE users SET trace_generation=trace_generation+1 WHERE id=$1 RETURNING trace_enabled,trace_retention_days,trace_generation`, user).Scan(&p.Enabled, &p.RetentionDays, &p.Generation); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM request_traces WHERE user_id=$1`, user)
		if err != nil {
			return err
		}
		traceDeleted = uint64(tag.RowsAffected())
		if activity {
			tag, err := tx.Exec(ctx, `DELETE FROM usage WHERE forwarded_for=$1 OR (user_id=$1 AND forwarded_for IS NULL)`, user)
			count = tag.RowsAffected()
			return err
		}
		return nil
	})
	if err == nil {
		s.Deleted.Add(traceDeleted)
		next := make(map[string]Preferences)
		if old := s.prefs.Load(); old != nil {
			for id, pref := range *old {
				next[id] = pref
			}
		}
		next[user] = p
		s.prefs.Store(&next)
	}
	return count, err
}

// SetEnabled is a site-wide kill switch; it waits for an in-progress write and
// invalidates all captures, including across disable/re-enable cycles.
func (s *Service) SetEnabled(enabled bool) {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.enabled = enabled
	s.epoch++
}
func (s *Service) Enabled() bool { s.gate.RLock(); defer s.gate.RUnlock(); return s.enabled }
