package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/groupsync"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestDiagnosticsReflectCurrentHealthAndSyncProgress(t *testing.T) {
	var database, listener atomic.Bool
	database.Store(true)
	listener.Store(true)
	stats := &groupsync.Stats{}
	stats.Enabled.Store(true)
	stats.IntervalSeconds.Store(900)
	stats.Record(groupsync.Result{}, nil)
	observer, err := New(t.Context(), Options{
		DatabaseHealth: func(ctx context.Context) bool {
			if _, bounded := ctx.Deadline(); !bounded {
				t.Error("database probe has no deadline")
			}
			return database.Load()
		}, InvalidationHealth: listener.Load, GroupSync: stats,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	values := func() map[string]float64 {
		t.Helper()
		families, err := observer.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		result := map[string]float64{}
		for _, family := range families {
			for _, sample := range family.Metric {
				if sample.Gauge != nil {
					result[family.GetName()] = sample.Gauge.GetValue()
				}
				if family.GetName() == "olr_group_sync_runs_total" {
					result["sync_"+sample.Label[0].GetValue()] = sample.Counter.GetValue()
				}
			}
		}
		return result
	}
	first := values()
	if first["olr_database_reachable"] != 1 || first["olr_invalidation_listener_connected"] != 1 || first["sync_success"] != 1 || first["olr_group_sync_last_success_timestamp_seconds"] == 0 {
		t.Fatal("healthy state missing", first)
	}
	database.Store(false)
	listener.Store(false)
	last := values()
	if last["olr_database_reachable"] != 0 || last["olr_invalidation_listener_connected"] != 0 {
		t.Fatal("health became stale")
	}
	if last["olr_group_sync_last_success_timestamp_seconds"] != first["olr_group_sync_last_success_timestamp_seconds"] {
		t.Fatal("scrape invented a sync success")
	}
}

func TestFirstByteHistogramRepresentsLongQueueWaits(t *testing.T) {
	observer, err := New(t.Context(), Options{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := observer.Start(httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("")), "openai")
	firstByte := int64(125000)
	observer.Finish(ctx, span, usage.Record{Model: "chat-model", Protocol: "openai", Status: "200", TTFBMS: &firstByte, LatencyMS: 130000}, 0, "chat")
	families, err := observer.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "olr_time_to_first_token_seconds" {
			for _, sample := range family.Metric {
				for _, bucket := range sample.Histogram.Bucket {
					if bucket.GetUpperBound() > 125 && bucket.GetCumulativeCount() == 1 {
						return
					}
				}
			}
		}
	}
	t.Fatal("long first-byte measurement has no finite bucket")
}
