// Package activity streams live request metadata to the portal of the user a
// request is attributed to. Events carry no content: IDs, model, timing,
// byte counts and status, the same facts the usage list shows. Publishing
// never blocks the forwarding path; slow or absent subscribers lose events.
//
// Across router instances, lifecycle events (not progress ticks) are relayed
// through Postgres NOTIFY in small batches; every instance LISTENs and
// delivers them to its own subscribers.
package activity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	channel = "olr_activity"
	// Buffer is the per-subscription event buffer.
	Buffer = 64
	// MaxPerUser bounds a user's concurrent live connections per instance.
	MaxPerUser = 4
	// ProgressInterval throttles progress events per request.
	ProgressInterval = 500 * time.Millisecond
	relayBuffer      = 1024
	relayBatch       = 250 * time.Millisecond
	maxPayload       = 7000 // Postgres NOTIFY payloads must stay below 8000 bytes
)

type Event struct {
	Type         string    `json:"type"` // started, progress, finished, trace_ready
	RequestID    string    `json:"request_id"`
	Time         time.Time `json:"time,omitzero"`
	Model        string    `json:"model,omitempty"`
	Operation    string    `json:"operation,omitempty"`
	Protocol     string    `json:"protocol,omitempty"`
	Stream       bool      `json:"stream,omitempty"`
	ElapsedMS    int64     `json:"elapsed_ms,omitempty"`
	Bytes        int64     `json:"bytes,omitempty"`
	Writes       int       `json:"writes,omitempty"`
	Status       string    `json:"status,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	LatencyMS    int64     `json:"latency_ms,omitempty"`
	TTFBMS       *int64    `json:"ttfb_ms,omitempty"`
}

type Subscription struct {
	C    chan Event
	user string
}

type relayed struct {
	User  string `json:"u"`
	Event Event  `json:"e"`
}

type Broker struct {
	done     <-chan struct{}
	mu       sync.RWMutex
	subs     map[string]map[*Subscription]struct{}
	watching atomic.Int64
	relay    chan relayed
	instance string
	log      *slog.Logger
	wg       sync.WaitGroup
	// Dropped counts events a full subscription or relay buffer refused.
	Dropped atomic.Uint64
}

// New returns a broker whose subscriptions end when done closes. With a pool,
// it relays lifecycle events to and from other instances until done closes.
func New(done <-chan struct{}, pool *pgxpool.Pool, log *slog.Logger) *Broker {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	b := &Broker{done: done, subs: map[string]map[*Subscription]struct{}{}, instance: hex.EncodeToString(id), log: log}
	if pool != nil {
		b.relay = make(chan relayed, relayBuffer)
		ctx, cancel := context.WithCancel(context.Background())
		b.wg.Add(3)
		go func() { defer b.wg.Done(); <-done; cancel() }()
		go func() { defer b.wg.Done(); b.send(ctx, pool) }()
		go func() { defer b.wg.Done(); b.listen(ctx, pool) }()
	}
	return b
}

// Wait returns once the relay goroutines stopped after done closed.
func (b *Broker) Wait() {
	if b != nil {
		b.wg.Wait()
	}
}

func (b *Broker) Done() <-chan struct{} { return b.done }

// Subscribe registers a live connection, refusing more than MaxPerUser.
func (b *Broker) Subscribe(user string) (*Subscription, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs[user]) >= MaxPerUser {
		return nil, false
	}
	s := &Subscription{C: make(chan Event, Buffer), user: user}
	if b.subs[user] == nil {
		b.subs[user] = map[*Subscription]struct{}{}
	}
	b.subs[user][s] = struct{}{}
	b.watching.Add(1)
	return s, true
}

func (b *Broker) Unsubscribe(s *Subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s.user][s]; ok {
		delete(b.subs[s.user], s)
		if len(b.subs[s.user]) == 0 {
			delete(b.subs, s.user)
		}
		b.watching.Add(-1)
	}
}

// Watching reports whether this instance has a live connection for user.
func (b *Broker) Watching(user string) bool {
	if b == nil || b.watching.Load() == 0 {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[user]) > 0
}

// Publish delivers locally and, except for progress ticks, relays the event
// to other instances. It never blocks.
func (b *Broker) Publish(user string, e Event) {
	if b == nil || user == "" {
		return
	}
	b.deliver(user, e)
	if b.relay != nil && e.Type != "progress" {
		select {
		case b.relay <- relayed{User: user, Event: e}:
		default:
			b.Dropped.Add(1)
		}
	}
}

func (b *Broker) deliver(user string, e Event) {
	if b.watching.Load() == 0 {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs[user] {
		select {
		case s.C <- e:
		default:
			b.Dropped.Add(1)
		}
	}
}

type envelope struct {
	Instance string    `json:"i"`
	Events   []relayed `json:"e"`
}

func (b *Broker) send(ctx context.Context, pool *pgxpool.Pool) {
	var batch []relayed
	size := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		payload, _ := json.Marshal(envelope{Instance: b.instance, Events: batch})
		batch, size = nil, 0
		sendCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := pool.Exec(sendCtx, `SELECT pg_notify($1, $2)`, channel, string(payload)); err != nil && ctx.Err() == nil {
			b.Dropped.Add(1)
		}
	}
	timer := time.NewTimer(relayBatch)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-b.relay:
			encoded, _ := json.Marshal(r)
			if size+len(encoded) > maxPayload-64 {
				flush()
			}
			batch = append(batch, r)
			size += len(encoded) + 1
		case <-timer.C:
			flush()
			timer.Reset(relayBatch)
		}
	}
}

// listen holds one dedicated connection in LISTEN state, reconnecting with
// backoff. The connection is closed, never returned to the pool listening.
func (b *Broker) listen(ctx context.Context, pool *pgxpool.Pool) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := b.listenOnce(ctx, pool, &backoff)
		if ctx.Err() != nil {
			return
		}
		b.log.Warn("live activity listener reconnecting", "error_class", errorClass(err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (b *Broker) listenOnce(ctx context.Context, pool *pgxpool.Pool, backoff *time.Duration) error {
	pooled, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	*backoff = time.Second
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var env envelope
		if json.Unmarshal([]byte(n.Payload), &env) != nil || env.Instance == b.instance {
			continue
		}
		for _, r := range env.Events {
			b.deliver(r.User, r.Event)
		}
	}
}

func errorClass(err error) string {
	if err == nil {
		return "closed"
	}
	if err == context.DeadlineExceeded {
		return "timeout"
	}
	return "connection"
}
