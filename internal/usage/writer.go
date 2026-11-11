package usage

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const queueSize = 1024
const writerCount = 4
const writeTimeout = 2 * time.Second

type store interface {
	insert(context.Context, Record) error
}

// Counters counts successful transactions, failed transactions, and records
// dropped because the queue was full or closed. No failure reaches a client.
type Counters struct{ Written, Failed, Dropped uint64 }

type Writer struct {
	store   store
	log     *slog.Logger
	queue   chan Record
	mu      sync.RWMutex
	closed  bool
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	written atomic.Uint64
	failed  atomic.Uint64
	dropped atomic.Uint64
}

func NewWriter(pool *pgxpool.Pool, log *slog.Logger) *Writer {
	return newWriter(postgresStore{pool}, log, queueSize, writerCount)
}

func newWriter(store store, log *slog.Logger, capacity, workers int) *Writer {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Writer{store: store, log: log, queue: make(chan Record, capacity), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for record := range w.queue {
				ctx, cancel := context.WithTimeout(w.ctx, writeTimeout)
				err := w.store.insert(ctx, record)
				cancel()
				if err != nil {
					w.failed.Add(1)
					// Driver errors can contain SQL arguments. Log no error or
					// record content, only a generic failure and a counter.
					w.log.Error("usage write failed", "failed", w.failed.Load())
				} else {
					w.written.Add(1)
				}
			}
		})
	}
	go func() { wg.Wait(); close(w.done) }()
	return w
}

func (w *Writer) Record(record Record) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.closed {
		// Own the nullable timing value across the asynchronous boundary.
		if record.TTFBMS != nil {
			ttfb := *record.TTFBMS
			record.TTFBMS = &ttfb
		}
		select {
		case w.queue <- record:
			return
		default:
		}
	}
	w.dropped.Add(1)
	w.log.Warn("usage record dropped", "dropped", w.dropped.Load())
}

func (w *Writer) Counters() Counters {
	return Counters{Written: w.written.Load(), Failed: w.failed.Load(), Dropped: w.dropped.Load()}
}

// Close stops admission and drains pending records. A deadline cancels storage
// attempts, counts the failed writes, and bounds shutdown during database loss.
// Call after HTTP handlers finish; it is safe to call more than once.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	defer w.cancel()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		w.cancel()
		<-w.done
		return ctx.Err()
	}
}

type postgresStore struct{ pool *pgxpool.Pool }

func (s postgresStore) insert(ctx context.Context, r Record) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO usage
			(ts, key_id, user_id, model, replica, protocol, status, input_tokens, output_tokens, latency_ms, ttfb_ms)
			VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			r.StartedAt, r.KeyID, r.UserID, r.Model, r.Replica, r.Protocol, r.Status,
			r.InputTokens, r.OutputTokens, r.LatencyMS, r.TTFBMS)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO usage_daily (day, user_id, model, requests, input_tokens, output_tokens)
			VALUES ($1, $2, $3, 1, $4, $5)
			ON CONFLICT (day, user_id, model) DO UPDATE SET
			requests = usage_daily.requests + 1,
			input_tokens = usage_daily.input_tokens + EXCLUDED.input_tokens,
			output_tokens = usage_daily.output_tokens + EXCLUDED.output_tokens`,
			r.StartedAt.UTC().Format("2006-01-02"), r.UserID, r.Model, r.InputTokens, r.OutputTokens)
		return err
	})
}
