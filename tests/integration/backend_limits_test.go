package integration_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/backendlimits"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSharedBackendBudgetsAcrossRouterInstances(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	managers := []*backendlimits.Manager{{DB: pool}, {DB: openDatabase(t, database.dsn)}}
	var absent *backendlimits.Manager
	if lease, err := absent.Acquire(t.Context(), "unlimited", config.BackendRate{}, 1); lease != nil || err != nil {
		t.Fatal("unlimited requires no database", err)
	}
	rpm, tpm, concurrent := int64(1), int64(100), 3
	reject := func(err error, name string) {
		t.Helper()
		var rejection *limits.Rejection
		if !errors.As(err, &rejection) || rejection.Limit != "backend_"+name || rejection.RetryAfter < 1 {
			t.Errorf("wanted %s rejection, got %v", name, err)
		}
	}
	if _, err := absent.Acquire(t.Context(), "limited", config.BackendRate{RPM: &rpm}, 1); !errors.Is(err, limits.ErrUnavailable) {
		t.Fatal("limited backend did not fail closed")
	}
	lease, err := managers[0].Acquire(t.Context(), "rpm", config.BackendRate{RPM: &rpm}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Complete(2); err != nil {
		t.Fatal(err)
	}
	_, err = managers[1].Acquire(t.Context(), "rpm", config.BackendRate{RPM: &rpm}, 1)
	reject(err, "rpm")
	other, err := managers[1].Acquire(t.Context(), "other", config.BackendRate{RPM: &rpm}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Complete(0); err != nil {
		t.Fatal(err)
	}
	first, err := managers[0].Acquire(t.Context(), "tokens", config.BackendRate{TPM: &tpm}, 70)
	if err != nil {
		t.Fatal(err)
	}
	_, err = managers[1].Acquire(t.Context(), "tokens", config.BackendRate{TPM: &tpm}, 40)
	reject(err, "tpm")
	if err = first.Complete(20); err != nil {
		t.Fatal(err)
	}
	if err = first.Complete(99); err != nil {
		t.Fatal(err)
	}
	second, err := managers[1].Acquire(t.Context(), "tokens", config.BackendRate{TPM: &tpm}, 80)
	if err != nil {
		t.Fatal("reservation not released", err)
	}
	if err = second.Complete(80); err != nil {
		t.Fatal(err)
	}
	_, err = managers[0].Acquire(t.Context(), "tokens", config.BackendRate{TPM: &tpm}, 1)
	reject(err, "tpm")
	execSQL(t, pool, "UPDATE backend_rate_state SET window_start=window_start-interval '2 minutes' WHERE backend IN ('tokens','rpm')")
	reset, err := managers[0].Acquire(t.Context(), "tokens", config.BackendRate{TPM: &tpm}, 100)
	if err != nil {
		t.Fatal("minute did not roll over", err)
	}
	if err = reset.Complete(0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mutex sync.Mutex
	var leases []*backendlimits.Lease
	for i := range 24 {
		wg.Go(func() {
			l, err := managers[i%2].Acquire(t.Context(), "concurrency", config.BackendRate{MaxConcurrent: &concurrent}, 1)
			if err != nil {
				reject(err, "concurrency")
				return
			}
			mutex.Lock()
			leases = append(leases, l)
			mutex.Unlock()
		})
	}
	wg.Wait()
	if len(leases) != concurrent {
		t.Fatalf("shared concurrency = %d want %d", len(leases), concurrent)
	}
	for _, l := range leases {
		if err = l.Complete(0); err != nil {
			t.Fatal(err)
		}
	}
	released, err := managers[0].Acquire(t.Context(), "concurrency", config.BackendRate{MaxConcurrent: &concurrent}, 1)
	if err != nil {
		t.Fatal("concurrency leaked", err)
	}
	execSQL(t, pool, "UPDATE backend_rate_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE backend='concurrency'")
	replacement, err := managers[1].Acquire(t.Context(), "concurrency", config.BackendRate{MaxConcurrent: new(1)}, 1)
	if err != nil {
		t.Fatal("expired lease retained capacity", err)
	}
	if err = released.Complete(0); err != nil {
		t.Fatal(err)
	}
	if err = replacement.Complete(0); err != nil {
		t.Fatal(err)
	}
}

func TestBackendConcurrencyReleasedOnStreamDisconnect(t *testing.T) {
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}), usageOptions{configure: func(cfg *config.Config, h *proxy.Handler, _ *server.Server, pool *pgxpool.Pool) {
		model := cfg.Models["chat-model"]
		model.Pool[0].Backend = "gpu"
		cfg.Models["chat-model"] = model
		cfg.Backends = map[string]config.BackendPolicy{"gpu": {Groups: []string{"staff"}, Rate: config.BackendRate{MaxConcurrent: new(1)}}}
		h.BackendLimits = &backendlimits.Manager{DB: pool}
		publish, _, err := h.PrepareRouting(t.Context(), cfg, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		publish()
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", f.router.URL+"/v1/chat/completions", strings.NewReader("{\"model\":\"chat-model\",\"messages\":[],\"stream\":true}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		t.Fatal(response.StatusCode)
	}
	blocked := f.call(t, f.key, "POST", "/v1/chat/completions", "{\"model\":\"chat-model\",\"messages\":[]}")
	_, _ = io.Copy(io.Discard, blocked.Body)
	blocked.Body.Close()
	if blocked.StatusCode != 429 {
		t.Fatal("backend concurrency not enforced", blocked.StatusCode)
	}
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var active int
		if err = f.pool.QueryRow(t.Context(), "SELECT count(*) FROM backend_rate_leases WHERE backend='gpu'").Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream disconnect leaked backend capacity")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
