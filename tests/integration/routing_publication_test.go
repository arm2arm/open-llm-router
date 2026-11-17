package integration_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type delayedEndpointPublication struct {
	*proxy.Handler
	committed chan struct{}
	resume    chan struct{}
}

func (d *delayedEndpointPublication) Prepare(ctx context.Context, cfg *config.Config, model string) (func(), []pool.State, error) {
	publish, states, err := d.Handler.Prepare(ctx, cfg, model)
	if err != nil {
		return nil, states, err
	}
	return func() { close(d.committed); <-d.resume; publish() }, states, nil
}

func TestCommittedRoutingActivationIgnoresLaterResponseReadFailure(t *testing.T) {
	base := openDatabase(t, newDatabase(t).dsn)
	pc := base.Config()
	pc.MaxConns = 1
	var armed atomic.Bool
	var cancel context.CancelFunc
	var canceled chan struct{}
	pc.AfterRelease = func(*pgx.Conn) bool {
		if armed.CompareAndSwap(true, false) {
			cancel()
			close(canceled)
		}
		return true
	}
	database, err := pgxpool.NewWithConfig(t.Context(), pc)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	cfg, err := config.Parse([]byte("models: {}\ngroups: {}\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	s := settings.RoutingService{DB: database, File: cfg}
	actor := audit.Actor{Type: "operator_cli", Name: "commit-response-test"}
	v, err := s.Import(t.Context(), settings.DomainRouting, actor)
	if err != nil {
		t.Fatal(err)
	}
	original := v.Active.ID
	doc := *v.Active.Document
	doc.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
	data, _ := json.Marshal(doc)
	v, err = s.SaveDraft(t.Context(), settings.DomainRouting, v.Version, data, nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	draft := v.Draft.ID
	for _, rollback := range []bool{false, true} {
		waitFor(t, func() bool { return database.Stat().IdleConns() == 1 })
		ctx, stop := context.WithCancel(t.Context())
		cancel = stop
		canceled = make(chan struct{})
		armed.Store(true)
		target := draft
		if rollback {
			target = original
			v, err = s.Rollback(ctx, settings.DomainRouting, v.Version, target, actor)
		} else {
			v, err = s.Activate(ctx, settings.DomainRouting, v.Version, target, actor, false)
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("post-commit cancellation hook did not run")
		}
		stop()
		if err != nil || v.Active == nil || v.Active.ID != target {
			t.Fatalf("committed action reported failure: %v %+v", err, v)
		}
		var active int64
		if err := base.QueryRow(t.Context(), `SELECT active_revision FROM settings_state WHERE domain='routing'`).Scan(&active); err != nil || active != target {
			t.Fatal("response differs from committed revision", active, err)
		}
	}
}

func TestEndpointSaveCannotOverwriteConcurrentManagedActivation(t *testing.T) {
	var endpoint *endpoints.Service
	var runtime *proxy.Handler
	delayed := &delayedEndpointPublication{committed: make(chan struct{}), resume: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(delayed.resume) }) }
	defer unblock()
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, db *pgxpool.Pool, p *proxy.Handler) {
		cfg.OIDC.SyncIntervalS = 900
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: 90, UsageDailyDays: 400, RunAt: "03:00"}
		destinations, err := backendpolicy.New(cfg, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		delayed.Handler = p
		endpoint = &endpoints.Service{DB: db, Base: cfg, Policy: destinations, Runtime: delayed}
		h.Endpoints = endpoint
		runtime = p
		h.Routing = &settings.RoutingService{DB: db, File: cfg, Tokens: destinations.Token, Verify: endpoints.VerifyDestinations(destinations), Current: func() *config.Config { c, _ := p.Snapshot(); return c }}
		h.RoutingRuntime = p
	}})
	browser := signInLocalAdmin(t, f)
	candidate := f.cfg.Models["chat-model"]
	candidate.UpstreamModel = "old-endpoint-candidate"
	done := make(chan error, 1)
	go func() { _, err := endpoint.Save(t.Context(), "chat-model", 0, candidate, -1, f.owner); done <- err }()
	select {
	case <-delayed.committed:
	case err := <-done:
		t.Fatal("endpoint save did not reach committed stage", err)
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint save did not commit")
	}
	path := "/portal/api/admin/settings/routing"
	var out routingResponse
	if err := json.Unmarshal(adminCall(t, browser, "POST", path+"/import", map[string]any{}, 200), &out); err != nil {
		t.Fatal(err)
	}
	doc := *out.Settings.Active.Document
	staff := doc.Groups["staff"]
	staff.RPM = limitPtr(1)
	doc.Groups["staff"] = staff
	_ = json.Unmarshal(adminCall(t, browser, "PUT", path+"/draft", map[string]any{"version": out.Settings.Version, "document": doc}, 200), &out)
	_ = json.Unmarshal(adminCall(t, browser, "POST", path+"/activate", map[string]any{"version": out.Settings.Version, "revision": out.Settings.Draft.ID}, 200), &out)
	active := out.Settings.Active.ID
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("committed endpoint save failed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("endpoint publication did not finish")
	}
	running, _ := runtime.Snapshot()
	if running.Groups["staff"].RPM == nil || *running.Groups["staff"].RPM != 1 || running.Models["chat-model"].UpstreamModel == "old-endpoint-candidate" {
		t.Fatal("late endpoint publication replaced managed routing")
	}
	var stored int64
	if err := f.pool.QueryRow(t.Context(), `SELECT active_revision FROM settings_state WHERE domain='routing'`).Scan(&stored); err != nil || stored != active || out.Runtime.Revision != active {
		t.Fatal("runtime and database revisions diverged", stored, active, err)
	}
}
