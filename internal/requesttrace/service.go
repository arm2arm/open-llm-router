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

	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxInput bounds the bytes copied from the request. Binary payloads are
	// removed in the worker; MaxInputText then bounds the normalized text.
	MaxInput     = 1 << 20
	MaxInputText = 384 << 10
	MaxRaw       = 128 << 10
	MaxFrames    = 200
	MaxFrameRaw  = 64 << 10
	MaxOutput    = 256 << 10
	MaxChunks    = 2048
	MaxEvents    = 1000
	// Slots cover active captures AND the background queue, not just queued work.
	Slots = 32
)

type Preferences struct {
	Enabled       bool  `json:"enabled"`
	RetentionDays int   `json:"retention_days"`
	Generation    int64 `json:"-"`
}

func defaults() Preferences { return Preferences{Enabled: true, RetentionDays: 7} }

type Service struct {
	pool      *pgxpool.Pool
	log       *slog.Logger
	mu        sync.Mutex
	prefs     atomic.Pointer[map[string]Preferences]
	slots     chan struct{}
	queue     chan *Capture
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	Processed atomic.Uint64
	Dropped   atomic.Uint64
	Failed    atomic.Uint64
}

func New(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*Service, error) {
	s := &Service{pool: pool, log: log, slots: make(chan struct{}, Slots), queue: make(chan *Capture, Slots)}
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
					<-s.slots
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
	rows, err := s.pool.Query(ctx, `SELECT id::text, trace_enabled, trace_retention_days, trace_generation FROM users WHERE trace_generation<>0 OR NOT trace_enabled OR trace_retention_days<>7`)
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
	err := s.pool.QueryRow(ctx, `SELECT trace_enabled, trace_retention_days, trace_generation FROM users WHERE id=$1`, user).Scan(&p.Enabled, &p.RetentionDays, &p.Generation)
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
		if err := tx.QueryRow(ctx, `UPDATE users SET trace_enabled=$2, trace_retention_days=$3, trace_generation=trace_generation+1 WHERE id=$1 RETURNING trace_generation`, user, p.Enabled, p.RetentionDays).Scan(&p.Generation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE request_traces SET expires_at=LEAST(expires_at,started_at+make_interval(days=>$2)) WHERE user_id=$1 AND expires_at>started_at+make_interval(days=>$2)`, user, p.RetentionDays)
		return err
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
	service       *Service
	generation    int64
	operation     string
	session       string
	routing       Routing
	input, output []byte
	chunks        []chunk
	truncated     bool
	outputStopped bool
	jsonResponse  bool
	began         time.Time
	record        usage.Record
}

// Begin never performs network I/O or waits for capacity. Unknown (new) users
// inherit enabled/7 days; the writer rechecks the database before storage.
func (s *Service) Begin(user, operation string, began time.Time) *Capture {
	if s == nil || user == "" {
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
	select {
	case s.slots <- struct{}{}:
		return &Capture{service: s, generation: p.Generation, operation: operation, began: began}
	default:
		s.Dropped.Add(1)
		return nil
	}
}

func (c *Capture) Input(p []byte) {
	if c == nil {
		return
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
		<-c.service.slots
	}
}

func (s *Service) persist(ctx context.Context, c *Capture) error {
	detail := c.decode()
	data, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	summary, err := json.Marshal(detail.Summary)
	if err != nil {
		return err
	}
	var conversation *string
	if key := c.conversationKey(detail); key != "" {
		conversation = &key
	}
	// FOR SHARE serializes against settings changes, erasure and user deletion.
	// Generation prevents late writes from resurrecting cleared content.
	_, err = s.pool.Exec(ctx, `WITH owner AS (
		SELECT id,trace_retention_days FROM users WHERE id=$2 AND trace_enabled AND trace_generation=$5 FOR SHARE
	) INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail,summary,conversation_key)
	SELECT $1,id,$3,$3::timestamptz+make_interval(days=>trace_retention_days),$4::jsonb,$6::jsonb,$7 FROM owner
	WHERE $3::timestamptz+make_interval(days=>trace_retention_days)>now()
	ON CONFLICT(request_id) DO NOTHING`, c.record.RequestID, c.record.UserID, c.record.StartedAt, data, c.generation, summary, conversation)
	return err
}

func (s *Service) Prune(ctx context.Context) error {
	// Each transaction is bounded. Access checks exclude expired content even
	// before physical cleanup; a backlog drains over successive ticks.
	_, err := s.pool.Exec(ctx, `DELETE FROM request_traces WHERE request_id IN (SELECT request_id FROM request_traces WHERE expires_at<=now() LIMIT 1000)`)
	return err
}

// Erase serializes with writers without changing a setting saved in another tab.
func (s *Service) Erase(ctx context.Context, user string, activity bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p Preferences
	var count int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE users SET trace_generation=trace_generation+1 WHERE id=$1 RETURNING trace_enabled,trace_retention_days,trace_generation`, user).Scan(&p.Enabled, &p.RetentionDays, &p.Generation); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM request_traces WHERE user_id=$1`, user); err != nil {
			return err
		}
		if activity {
			tag, err := tx.Exec(ctx, `DELETE FROM usage WHERE forwarded_for=$1 OR (user_id=$1 AND forwarded_for IS NULL)`, user)
			count = tag.RowsAffected()
			return err
		}
		return nil
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
	return count, err
}
