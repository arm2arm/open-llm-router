package integration_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/requesttrace"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTraceUpgradeRequiresProvenConsentAfterLegacyErasure(t *testing.T) {
	database := newDatabase(t)
	legacy, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	execSQL(t, legacy, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations()[:13] {
		execSQL(t, legacy, migration.SQL)
		execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	erased := user(t, legacy, "legacy", "erasure-only")
	oldChoice := user(t, legacy, "legacy", "old-save")
	implicit := user(t, legacy, "legacy", "implicit")
	execSQL(t, legacy, `UPDATE users SET trace_generation=1,trace_retention_days=21 WHERE id IN ($1,$2)`, erased, oldChoice)
	// Reproduce the old migration's false proof: erasure increments the same
	// counter as Save, so migration 0014 leaves both enabled.
	m14 := db.Migrations()[13]
	execSQL(t, legacy, m14.SQL)
	execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, m14.Version, m14.Name)
	var enabled bool
	if err := legacy.QueryRow(t.Context(), `SELECT trace_enabled FROM users WHERE id=$1`, erased).Scan(&enabled); err != nil || !enabled {
		t.Fatal("legacy erasure fixture did not reproduce", enabled, err)
	}
	upgraded := openDatabase(t, database.dsn)
	for _, id := range []string{erased, oldChoice, implicit} {
		var updated *time.Time
		if err := upgraded.QueryRow(t.Context(), `SELECT trace_enabled,trace_preferences_set_at FROM users WHERE id=$1`, id).Scan(&enabled, &updated); err != nil || enabled || updated != nil {
			t.Fatal("upgrade invented explicit consent", id, enabled, updated, err)
		}
	}
	var days int
	if err := upgraded.QueryRow(t.Context(), `SELECT trace_retention_days FROM users WHERE id=$1`, erased).Scan(&days); err != nil || days != 21 {
		t.Fatal("upgrade lost retention choice", days, err)
	}
}

func TestTracePreferenceProvenanceAndErasureAreIndependent(t *testing.T) {
	f := startPortal(t)
	browser := f.browser(t)
	id := browser.login(t, "trace-consent", []string{"staff"})
	get := func() requesttrace.Preferences {
		t.Helper()
		p, err := f.traces.Preferences(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := get(); p.Enabled || p.UpdatedAt != nil {
		t.Fatal("new account implicitly opted in", p)
	}
	// Erasure alone remains independent of an explicit preference choice.
	if _, err := f.traces.Erase(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if p := get(); p.Enabled || p.UpdatedAt != nil {
		t.Fatal("erasure became consent", p)
	}
	save := func(body string) requesttrace.Preferences {
		t.Helper()
		r, data := browser.call(t, "PUT", "/portal/api/trace-preferences", body, f.cfg.Portal.BaseURL)
		var p requesttrace.Preferences
		if r.StatusCode != 200 || json.Unmarshal(data, &p) != nil || p.UpdatedAt == nil {
			t.Fatal("preference timestamp not returned", r.StatusCode, string(data))
		}
		return p
	}
	first := save(`{"enabled":true,"retention_days":7}`)
	if !first.Enabled {
		t.Fatal("explicit choice not saved")
	}
	beforeErase := get()
	if _, err := f.traces.Erase(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	afterErase := get()
	if !afterErase.Enabled || !first.UpdatedAt.Equal(*afterErase.UpdatedAt) || afterErase.Generation <= beforeErase.Generation {
		t.Fatal("erasure changed consent or failed to invalidate captures", first, afterErase)
	}
	second := save(`{"enabled":false,"retention_days":7}`)
	if second.Enabled || !second.UpdatedAt.After(*first.UpdatedAt) {
		t.Fatal("withdrawal timestamp not recorded", second)
	}
	repeat := save(`{"enabled":false,"retention_days":7}`)
	if !repeat.UpdatedAt.Equal(*second.UpdatedAt) {
		t.Fatal("unchanged save fabricated a new choice")
	}
	third := save(`{"enabled":false,"retention_days":3}`)
	if !third.UpdatedAt.After(*second.UpdatedAt) {
		t.Fatal("retention change not timestamped")
	}
	rows, err := f.pool.Query(t.Context(), `SELECT actor_type,actor_id::text,target_id,details FROM audit_events WHERE action='trace.preferences' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var actorType, actor, target string
		var detail struct {
			Enabled   bool       `json:"enabled"`
			Days      int        `json:"retention_days"`
			UpdatedAt *time.Time `json:"updated_at"`
		}
		var raw []byte
		if err := rows.Scan(&actorType, &actor, &target, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatal(err)
		}
		if count >= 3 || actorType != "portal_user" || actor != id || target != id || detail.UpdatedAt == nil || detail.Enabled != (count == 0) || detail.Days != []int{7, 7, 3}[count] {
			t.Fatal("incomplete preference history", actor, target, string(raw))
		}
		count++
	}
	if rows.Err() != nil || count != 3 {
		t.Fatal("preference history should contain changes only", count, rows.Err())
	}
	rows.Close()
	// A preference cannot commit if its provenance record cannot be saved.
	execSQL(t, f.pool, `ALTER TABLE audit_events ADD CONSTRAINT reject_trace_preference_test CHECK (action<>'trace.preferences') NOT VALID`)
	if err := f.traces.Save(t.Context(), id, requesttrace.Preferences{Enabled: true, RetentionDays: 3}); err == nil {
		t.Fatal("setting committed without consent history")
	}
	if current := get(); current.Enabled || !current.UpdatedAt.Equal(*third.UpdatedAt) {
		t.Fatal("failed history write changed the preference", current)
	}
}
