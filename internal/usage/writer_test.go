package usage

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	insertFunc func(context.Context, Record) error
}

func (s fakeStore) insert(ctx context.Context, r Record) error { return s.insertFunc(ctx, r) }

func TestWriterQueueDoesNotBlockAndCloseDrains(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var rows []Record
	store := fakeStore{func(ctx context.Context, r Record) error {
		if len(rows) == 0 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		rows = append(rows, r)
		return nil
	}}
	w := newWriter(store, slog.New(slog.DiscardHandler), 1, 1)
	w.Record(Record{Model: "first"})
	<-entered
	ttfb := int64(7)
	w.Record(Record{Model: "second", TTFBMS: &ttfb})
	ttfb = 99
	finished := make(chan struct{})
	go func() { w.Record(Record{Model: "overflow"}); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("full queue blocked an inference handler")
	}
	close(release)
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counts := w.Counters(); counts.Written != 2 || counts.Failed != 0 || counts.Dropped != 1 || len(rows) != 2 || *rows[1].TTFBMS != 7 {
		t.Fatalf("counts=%+v rows=%+v", counts, rows)
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal("repeated close failed", err)
	}
}

func TestWriterFailuresArePrivateAndCounted(t *testing.T) {
	var logs bytes.Buffer
	store := fakeStore{func(context.Context, Record) error { return errors.New("secret-prompt-marker in driver error") }}
	w := newWriter(store, slog.New(slog.NewJSONHandler(&logs, nil)), 2, 1)
	w.Record(Record{Model: "chat-model"})
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if counts := w.Counters(); counts.Failed != 1 || counts.Written != 0 || !bytes.Contains(logs.Bytes(), []byte("usage write failed")) || bytes.Contains(logs.Bytes(), []byte("secret-prompt-marker")) {
		t.Fatalf("counts=%+v logs=%s", counts, &logs)
	}
}

func TestWriterShutdownDeadline(t *testing.T) {
	store := fakeStore{func(ctx context.Context, _ Record) error { <-ctx.Done(); return ctx.Err() }}
	w := newWriter(store, slog.New(slog.DiscardHandler), 2, 1)
	w.Record(Record{})
	w.Record(Record{})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := w.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("shutdown did not report its deadline", err)
	}
	if counts := w.Counters(); counts.Failed != 2 || counts.Written != 0 || counts.Dropped != 0 {
		t.Fatal(counts)
	}
}

func TestWriterConcurrentCloseAndAdmission(t *testing.T) {
	store := fakeStore{func(context.Context, Record) error { return nil }}
	w := newWriter(store, slog.New(slog.DiscardHandler), 128, 4)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { w.Record(Record{}) })
	}
	wg.Go(func() {
		if err := w.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	if counts := w.Counters(); counts.Written+counts.Dropped != 100 || counts.Failed != 0 {
		t.Fatal(counts)
	}
}
