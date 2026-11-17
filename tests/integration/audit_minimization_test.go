package integration_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuditSourceMinimizationUpgradeAndInsertGuards(t *testing.T) {
	database := newDatabase(t)
	legacy, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	execSQL(t, legacy, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations()[:26] {
		execSQL(t, legacy, migration.SQL)
		execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	owner := user(t, legacy, "legacy", "source-owner")
	key, _, err := adminaccess.Issue(t.Context(), legacy, owner, adminaccess.Create{Label: "source fixture", Permissions: []string{"service:read"}, Networks: []string{"127.0.0.1/32"}, Days: 1}, nil, 10, audit.Actor{Type: "operator_cli"})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, legacy, `UPDATE admin_api_keys SET last_ip='141.33.4.201' WHERE id=$1`, key.ID)
	cases := []struct{ raw, want string }{{"141.33.4.201", "141.33.4.0/24"}, {"2001:db8:abcd:1234::42", "2001:db8:abcd::/48"}, {"::ffff:141.33.4.201", "141.33.4.0/24"}, {"invalid", ""}}
	insert := func(pool *pgxpool.Pool, phase string) {
		for i, tc := range cases {
			execSQL(t, pool, `INSERT INTO audit_events(action,actor_type,target_id,result,details) VALUES('agent.read','admin_api_key',$1,'succeeded',jsonb_build_object('agent',jsonb_build_object('source_ip',$2::text,'transport','rest')))`, fmt.Sprintf("%s-%d", phase, i), tc.raw)
		}
	}
	insert(legacy, "old")
	upgraded := openDatabase(t, database.dsn)
	insert(upgraded, "new")
	for _, phase := range []string{"old", "new"} {
		for i, tc := range cases {
			var got, transport string
			if err := upgraded.QueryRow(t.Context(), `SELECT COALESCE(details#>>'{agent,source_ip}',''),details#>>'{agent,transport}' FROM audit_events WHERE target_id=$1`, fmt.Sprintf("%s-%d", phase, i)).Scan(&got, &transport); err != nil || got != tc.want || transport != "rest" {
				t.Fatal("audit source retained host bits or lost other attribution", phase, i, got, err)
			}
		}
	}
	var address string
	if err := upgraded.QueryRow(t.Context(), `SELECT last_ip FROM admin_api_keys WHERE id=$1`, key.ID).Scan(&address); err != nil || address != "141.33.4.0/24" {
		t.Fatal("old key address not minimized", address, err)
	}
	execSQL(t, upgraded, `UPDATE admin_api_keys SET last_ip='2001:db8:abcd:1234::42' WHERE id=$1`, key.ID)
	if err := upgraded.QueryRow(t.Context(), `SELECT last_ip FROM admin_api_keys WHERE id=$1`, key.ID).Scan(&address); err != nil || address != "2001:db8:abcd::/48" {
		t.Fatal("new key address not minimized", address, err)
	}
	if _, err := upgraded.Exec(t.Context(), `UPDATE audit_events SET reason='tampered'`); err == nil {
		t.Fatal("append-only guard was not restored")
	}
	if _, err := upgraded.Exec(t.Context(), `TRUNCATE audit_events`); err == nil {
		t.Fatal("truncate guard was lost")
	}
}

func TestIdenticalPortalSignInsDoNotAuditOrInvalidatePreviews(t *testing.T) {
	f := startPortal(t)
	id := f.browser(t).login(t, "repeat-identity", []string{"staff"})
	read := func() (int, int64, time.Time) {
		t.Helper()
		var count int
		var version int64
		var synced time.Time
		if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM audit_events WHERE action='user.identity_sync' AND target_id=$1::text),(SELECT version FROM admin_state),synced_at FROM users WHERE id=$1::uuid`, id).Scan(&count, &version, &synced); err != nil {
			t.Fatal(err)
		}
		return count, version, synced
	}
	count, version, synced := read()
	if count != 1 {
		t.Fatal("first sign-in missing history", count)
	}
	if got := f.browser(t).login(t, "repeat-identity", []string{"staff", "staff"}); got != id {
		t.Fatal("identity changed")
	}
	count2, version2, synced2 := read()
	if count2 != count || version2 != version || !synced2.Equal(synced) {
		t.Fatal("identical claims changed audit/state/timestamp", count2, version2)
	}
	f.browser(t).login(t, "repeat-identity", []string{"staff", "admins"})
	count3, version3, synced3 := read()
	if count3 != 2 || version3 <= version2 || !synced3.After(synced2) {
		t.Fatal("changed groups did not audit/invalidate", count3, version3)
	}
}

func TestConcurrentIdenticalIdentitySyncAuditsOnce(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	admin := auth.Admin{Pool: pool}
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := admin.SyncIdentity(t.Context(), "https://identity.example", "same-sub", "same-name", "same@example.org", []string{"staff"}, true)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='user.identity_sync'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent claims duplicated identity audit", count, err)
	}
}
