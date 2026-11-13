package integration_test

import (
	"encoding/json"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdministrationUpgradePreservesCredentialsPoliciesAndHistory(t *testing.T) {
	database := newDatabase(t)
	previous, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(previous.Close)
	execSQL(t, previous, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations() {
		if migration.Version >= 15 {
			break
		}
		execSQL(t, previous, migration.SQL)
		execSQL(t, previous, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	admin := auth.Admin{Pool: previous}
	root, _, err := admin.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ChangeLocalPassword(t.Context(), root.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (string, bool) { return "", false }
	cfg, err := config.Parse([]byte(`models:
  m:
    protocols: [openai]
    timeout_s: 5
    pool: [{url: "http://backend:8000", weight: 1, max_inflight: 4}]
groups:
  staff: {models: [m], rpm: 7}
`), lookup)
	if err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: "operator_cli", Name: "upgrade-test"}
	routing := settings.RoutingService{DB: previous, File: cfg, Lookup: lookup}
	view, err := routing.Import(t.Context(), settings.DomainRouting, actor)
	if err != nil {
		t.Fatal(err)
	}
	doc := *view.Active.Document
	staff := doc.Groups["staff"]
	rpm := 11
	staff.RPM = &rpm
	doc.Groups["staff"] = staff
	encoded, _ := json.Marshal(doc)
	view, err = routing.SaveDraft(t.Context(), settings.DomainRouting, view.Version, encoded, nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	view, err = routing.Activate(t.Context(), settings.DomainRouting, view.Version, view.Draft.ID, actor, false)
	if err != nil {
		t.Fatal(err)
	}
	authentication := settings.AuthenticationService{DB: previous, File: cfg, Lookup: lookup}
	if _, err := authentication.Import(t.Context(), settings.DomainAuthentication, actor); err != nil {
		t.Fatal(err)
	}
	// Compare opaque snapshots without ever writing credential hashes to test
	// output. Existing events and active/draft/version pointers must be exact.
	snapshot := func(pool *pgxpool.Pool) string {
		t.Helper()
		var value string
		err := pool.QueryRow(t.Context(), `SELECT json_build_object(
 'credentials',(SELECT json_agg(row_to_json(c) ORDER BY user_id) FROM local_credentials c),
 'protected',(SELECT json_agg(row_to_json(u) ORDER BY id) FROM users u WHERE protected_admin),
 'state',(SELECT json_agg(row_to_json(s) ORDER BY domain) FROM settings_state s),
 'revisions',(SELECT md5(string_agg(row_to_json(r)::text,'' ORDER BY id)) FROM settings_revisions r),
 'audit',(SELECT md5(string_agg(row_to_json(a)::text,'' ORDER BY id)) FROM audit_events a))::text`).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot(previous)
	upgraded := openDatabase(t, database.dsn)
	if snapshot(upgraded) != before {
		t.Fatal("upgrade changed credentials, identities, settings or existing history")
	}
	var version int
	if err := upgraded.QueryRow(t.Context(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version < 15 {
		t.Fatal("administration migration not applied", err)
	}
	verified, err := (&auth.Admin{Pool: upgraded}).VerifyLocalPassword(t.Context(), "admin", []byte(rotatedAdminPassword))
	if err != nil || verified.ID != root.ID || verified.MustChangePassword {
		t.Fatal("upgraded administrator cannot sign in", err)
	}
	after, err := (&settings.RoutingService{DB: upgraded, File: cfg, Lookup: lookup}).View(t.Context(), settings.DomainRouting)
	if err != nil || after.Active.ID != view.Active.ID || *after.Active.Document.Groups["staff"].RPM != 11 {
		t.Fatal("upgrade lost active group policy", err)
	}
}
