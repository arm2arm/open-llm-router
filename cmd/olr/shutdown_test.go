package main

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestAccountingFlushPrecedesUnresponsiveTelemetry(t *testing.T) {
	var mu sync.Mutex
	var order []string
	appendStep := func(step string) { mu.Lock(); order = append(order, step); mu.Unlock() }
	accounted, exporting, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		flushAccountingAndTelemetry(slog.New(slog.DiscardHandler), func(ctx context.Context) error {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
				t.Error("missing accounting deadline")
			}
			appendStep("accounting")
			close(accounted)
			return nil
		}, func() { appendStep("content") }, func(ctx context.Context) error {
			appendStep("telemetry")
			close(exporting)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-accounted:
	case <-time.After(time.Second):
		t.Fatal("collector delayed accounting")
	}
	<-exporting
	select {
	case <-done:
		t.Fatal("collector did not exercise the shutdown timeout")
	default:
	}
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("telemetry shutdown was not bounded")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(order, []string{"accounting", "content", "telemetry"}) {
		t.Fatal(order)
	}
}
