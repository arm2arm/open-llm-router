package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/groupsync"
)

func TestManualGroupSyncUnavailableAndSwitching(t *testing.T) {
	s := &oidcServices{stats: &groupsync.Stats{}}
	if _, err := s.SyncGroups(t.Context()); !errors.Is(err, groupsync.ErrDisabled) {
		t.Fatal("unconfigured job was not refused", err)
	}
	s.stats.Enabled.Store(true)
	if _, err := s.SyncGroups(t.Context()); !errors.Is(err, groupsync.ErrUnavailable) {
		t.Fatal("initializing job was not reported", err)
	}
	s.mu.Lock()
	result, err := s.SyncGroups(t.Context())
	s.mu.Unlock()
	if err != nil || !result.Skipped {
		t.Fatal("manual sync waited behind a settings switch", result, err)
	}
}

type retryLog struct {
	record slog.Record
	cancel context.CancelFunc
}

func (h *retryLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *retryLog) Handle(_ context.Context, record slog.Record) error {
	h.record = record.Clone()
	h.cancel()
	return nil
}
func (h *retryLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *retryLog) WithGroup(string) slog.Handler      { return h }

func TestOIDCRetryLogsClassifiedCauseAndStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &retryLog{cancel: cancel}
	retryOIDC(ctx, slog.New(handler), "API tokens", func(context.Context) error {
		return fmt.Errorf("https://user:private@issuer.invalid: %w", context.DeadlineExceeded)
	})
	attrs := map[string]string{}
	handler.record.Attrs(func(attr slog.Attr) bool { attrs[attr.Key] = attr.Value.String(); return true })
	if attrs["cause"] != "request timed out" || attrs["service"] != "API tokens" {
		t.Fatal("retry omitted diagnostic cause", attrs)
	}
	if strings.Contains(fmt.Sprint(attrs), "private") || strings.Contains(fmt.Sprint(attrs), "issuer.invalid") {
		t.Fatal("provider details escaped")
	}
}
