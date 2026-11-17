package main

import (
	"context"
	"log/slog"
	"time"
)

// HTTP handlers have drained before this runs. Accounting gets its full budget
// before either optional content traces or an unavailable OTLP collector.
func flushAccountingAndTelemetry(log *slog.Logger, accounting func(context.Context) error, content func(), telemetry func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := accounting(ctx); err != nil {
		log.Error("usage shutdown flush incomplete")
	}
	cancel()
	if content != nil {
		content() // requesttrace.Service.Close has its own two-second budget
	}
	if telemetry != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetry(ctx); err != nil {
			log.Error("trace shutdown flush incomplete")
		}
	}
}
