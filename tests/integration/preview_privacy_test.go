package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/retention"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPreviewPrivacyErasureAndAutomaticRetention(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
	}})
	owner := f.browser(t).login(t, "preview-operator", []string{"admins"})
	key, secret, err := adminaccess.Issue(t.Context(), f.pool, owner, adminaccess.Create{Label: "preview operator", Permissions: adminaccess.Permissions, Networks: []string{"127.0.0.1/32"}, Days: 1}, nil, 10, audit.Actor{Type: "portal_user", ID: owner})
	if err != nil {
		t.Fatal(err)
	}
	admin := auth.Admin{Pool: f.pool}
	victim, err := admin.AddUser(t.Context(), "preview-private-username", "preview-private@example.org", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateKey(t.Context(), victim, "private-inference-key-label", time.Hour); err != nil {
		t.Fatal(err)
	}
	value := int64(5)
	if err := admin.SetOverride(t.Context(), victim, "tokens_per_day", &value, "private-old-override-reason", "test"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"operation": "set_user_override", "arguments": map[string]any{"parameters": map[string]string{"id": victim, "field": "tokens_per_day"}, "body": map[string]any{"value": 8, "reason": "private-new-override-reason"}}})
	req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/api/admin/v1/changes/preview", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	r, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != 201 {
		t.Fatal("prepare preview", r.StatusCode, err)
	}
	// The authorized caller still gets a useful immediate before/after view.
	if !strings.Contains(string(response), "preview-private-username") || !strings.Contains(string(response), "private-new-override-reason") {
		t.Fatal("immediate authorized preview lost its useful details")
	}
	assertPrivate := func() {
		t.Helper()
		var stored string
		if err := f.pool.QueryRow(t.Context(), `SELECT COALESCE(string_agg(summary::text,''),'') FROM admin_changes`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"preview-private-username", "preview-private@example.org", "private-inference-key-label", "private-old-override-reason", "private-new-override-reason"} {
			if strings.Contains(stored, private) {
				t.Fatal("preview persisted identity/label/free-text content")
			}
		}
		if !strings.Contains(stored, "field_names") {
			t.Fatal("minimal preview summary unavailable")
		}
	}
	assertPrivate()
	if err := admin.DeleteUser(t.Context(), victim); err != nil {
		t.Fatal(err)
	}
	assertPrivate()
	// The periodic job removes expired unused previews and old terminal/stuck
	// changes, while retaining fresh previews and useful idempotency results.
	for _, row := range []struct{ status, age, expiry string }{
		{"previewed", "0 days", "-1 minute"},
		{"succeeded", "2 days", "1 day"},
		{"running", "2 days", "1 day"},
		{"succeeded", "0 days", "1 day"},
	} {
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO admin_changes(key_id,operation,arguments_hash,state_version,policy_version,summary,status,created,expires) VALUES($1,'set_user_override',decode(repeat('00',32),'hex'),1,1,'{}',$2,now()-$3::interval,now()+$4::interval)`, key.ID, row.status, row.age, row.expiry); err != nil {
			t.Fatal(err)
		}
	}
	result, err := retention.Prune(t.Context(), f.pool, config.Retention{UsageDays: 90, UsageDailyDays: 400}, time.UTC, time.Now())
	if err != nil || result.AdminChanges != 3 {
		t.Fatal("preview retention", result, err)
	}
	var remaining int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM admin_changes`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatal("fresh preview/result lost", remaining, err)
	}
}

func TestPreviewUpgradeRemovesHistoricalIdentitySnapshots(t *testing.T) {
	database := newDatabase(t)
	legacy, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	execSQL(t, legacy, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations()[:24] {
		execSQL(t, legacy, migration.SQL)
		execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	owner := user(t, legacy, "legacy", "preview-owner")
	key, _, err := adminaccess.Issue(t.Context(), legacy, owner, adminaccess.Create{Label: "old key", Permissions: []string{"users:write"}, Networks: []string{"127.0.0.1/32"}, Days: 1}, nil, 10, audit.Actor{Type: "operator_cli"})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, legacy, `INSERT INTO admin_changes(key_id,operation,arguments_hash,state_version,policy_version,summary) VALUES($1,'set_user_override',decode(repeat('00',32),'hex'),1,1,'{"before":{"username":"erased-private-name","email":"erased-private@example.org"},"after":{"reason":"private-old-reason"}}')`, key.ID)
	upgraded := openDatabase(t, database.dsn)
	var stored string
	if err := upgraded.QueryRow(t.Context(), `SELECT summary::text FROM admin_changes`).Scan(&stored); err != nil || strings.Contains(stored, "private") || !strings.Contains(stored, "set_user_override") {
		t.Fatal("historical preview content survived upgrade", err)
	}
}
