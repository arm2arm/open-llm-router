package requesttrace

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestQuotaPolicyRejectsOverflowBeforeOpeningStorage(t *testing.T) {
	for _, policy := range []config.ContentTracing{
		{MaxStoredMiB: 1<<44 + 64}, // would wrap the shifted bytes back to a valid 64 MiB
		{MaxStoredMiB: -1},
		{MaxStoredTraces: config.MaxTraceStoredTraces + 1},
	} {
		service, err := New(t.Context(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), policy)
		if service != nil || !errors.Is(err, ErrQuota) {
			t.Fatal("invalid quota reached storage", service, err)
		}
	}
}
