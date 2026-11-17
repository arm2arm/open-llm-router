package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type historyQueryRecorder struct {
	pool  *pgxpool.Pool
	query string
	args  []any
}

func (r *historyQueryRecorder) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	r.query, r.args = query, append([]any(nil), args...)
	return r.pool.Query(ctx, query, args...)
}

func TestHistoryActorAndTargetFiltersUseTheirIndexes(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	const actor = "b3d43fd3-4297-468f-8c73-9f07cd7dc255"
	if err := audit.Record(t.Context(), pool, audit.Event{Action: "test.history", Actor: audit.Actor{Type: "portal_user", ID: actor}, TargetType: "user", TargetID: "rare-target", Result: audit.Succeeded}); err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `INSERT INTO audit_events(action,actor_type,actor_id,target_type,target_id,result) SELECT 'test.history','portal_user',gen_random_uuid(),'user','noise-'||n::text,'succeeded' FROM generate_series(1,50000) n`)
	execSQL(t, pool, `ANALYZE audit_events`)
	for _, test := range []struct {
		filter audit.Filter
		index  string
	}{
		{audit.Filter{ActorID: actor}, "audit_events_actor"},
		{audit.Filter{TargetID: "rare-target"}, "audit_events_target_id"},
	} {
		recorder := &historyQueryRecorder{pool: pool}
		page, err := audit.List(t.Context(), recorder, test.filter)
		if err != nil || len(page.Events) != 1 {
			t.Fatal("history lookup", len(page.Events), err)
		}
		var plan []byte
		if err := pool.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+recorder.query, recorder.args...).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(plan), test.index) || !strings.Contains(string(plan), "Index") {
			t.Fatal("history filter did not use its index", test.index, string(plan))
		}
	}
	if _, err := audit.List(t.Context(), pool, audit.Filter{ActorID: "invalid-uuid"}); err != audit.ErrFilter {
		t.Fatal("invalid actor should be a validation error", err)
	}
}

func TestRequestTimeDemotionRevocationIsAuditedAndStorageFailureIs503(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
	}})
	owner := f.browser(t).login(t, "demoted-file-admin", []string{"admins"})
	key, secret, err := adminaccess.Issue(t.Context(), f.pool, owner, adminaccess.Create{Label: "demotion fixture", Permissions: []string{"service:read"}, Networks: []string{"127.0.0.1/32"}, Days: 1}, nil, 10, audit.Actor{Type: "portal_user", ID: owner})
	if err != nil {
		t.Fatal(err)
	}
	call := func(want int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/api/admin/v1/identity", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		r, err := f.router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatal("demotion response", r.StatusCode, want)
		}
	}
	call(200)
	// A file configuration can remove group authority independently of user
	// updates and the managed-routing SQL revocation trigger.
	group := f.cfg.Groups["admins"]
	group.Admin = false
	f.cfg.Groups["admins"] = group
	execSQL(t, f.pool, `CREATE FUNCTION reject_test_revocation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic revocation outage'; END $$; CREATE TRIGGER reject_test_revocation BEFORE UPDATE ON admin_api_keys FOR EACH ROW EXECUTE FUNCTION reject_test_revocation()`)
	call(503)
	var revoked bool
	if err := f.pool.QueryRow(t.Context(), `SELECT revoked FROM admin_api_keys WHERE id=$1`, key.ID).Scan(&revoked); err != nil || revoked {
		t.Fatal("failed revocation was not rolled back", revoked, err)
	}
	execSQL(t, f.pool, `DROP TRIGGER reject_test_revocation ON admin_api_keys`)
	call(401)
	call(401)
	var target, reason string
	var detail []byte
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='admin_key.revoke' AND target_id=$1`, key.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("demotion revocation missing or duplicated", count, err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT target_id,reason,details FROM audit_events WHERE action='admin_key.revoke' AND target_id=$1`, key.ID).Scan(&target, &reason, &detail); err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if json.Unmarshal(detail, &details) != nil || target != key.ID || reason != "owner_no_longer_admin" || details["owner_id"] != owner {
		t.Fatal("revocation lacks key, owner or policy reason")
	}
}
