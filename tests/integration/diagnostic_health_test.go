package integration_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/metrics"
	"github.com/arm2arm/open-llm-router/internal/server"
)

func TestReadinessDiagnosticsExposeDatabaseAndListenerOutages(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	authenticator, err := auth.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()
	ready := func(ctx context.Context) bool { return pool.Ping(ctx) == nil }
	observer, err := metrics.New(t.Context(), metrics.Options{Token: "synthetic-health-scrape", DatabaseHealth: ready,
		InvalidationHealth: func() bool { _, connected := authenticator.Freshness(); return connected },
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.SetReadiness(ready)
	srv.Handle("GET /metrics", observer.Handler())
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	get := func(path string, status int) string {
		t.Helper()
		request, _ := http.NewRequest("GET", httpServer.URL+path, nil)
		request.Header.Set("Authorization", "Bearer synthetic-health-scrape")
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != status {
			t.Fatalf("%s status=%d want=%d", path, response.StatusCode, status)
		}
		return string(body)
	}
	get("/readyz", 200)
	body := get("/metrics", 200)
	if !strings.Contains(body, "olr_database_reachable 1") || !strings.Contains(body, "olr_invalidation_listener_connected 1") {
		t.Fatal("healthy diagnostics missing")
	}
	authenticator.Close()
	if !strings.Contains(get("/metrics", 200), "olr_invalidation_listener_connected 0") {
		t.Fatal("listener outage not exposed")
	}
	pool.Close()
	get("/readyz", 503)
	if !strings.Contains(get("/metrics", 200), "olr_database_reachable 0") {
		t.Fatal("database outage hidden by successful metrics scrape")
	}
}
