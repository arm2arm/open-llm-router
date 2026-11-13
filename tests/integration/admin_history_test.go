package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/settings"
)

func TestAdministrationHistoryAPIAndCLI(t *testing.T) {
	f := startLocalPortal(t, nil)
	a := auth.Admin{Pool: f.pool}
	root, _, err := a.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ChangeLocalPassword(t.Context(), root.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	b := f.browser(t)
	b.localLogin(t, "admin", rotatedAdminPassword, 200)
	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		encoded, _ := json.Marshal(body)
		resp, data := b.call(t, method, path, string(encoded), f.cfg.Portal.BaseURL)
		if resp.StatusCode != want {
			t.Fatalf("%s %d want %d: %s", path, resp.StatusCode, want, data)
		}
		return data
	}
	data := call("POST", "/portal/api/admin/users", map[string]any{"username": "history-user", "password": initialAdminPassword, "groups": []string{"staff"}}, 201)
	var created struct{ ID string }
	_ = json.Unmarshal(data, &created)
	if err = a.ChangeLocalPassword(t.Context(), created.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	key, err := a.CreateKey(t.Context(), created.ID, "history-secret-label-not-retained", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := a.ListKeys(t.Context(), created.ID)
	if err != nil || len(keys) != 1 {
		t.Fatal(err)
	}
	call("DELETE", "/portal/api/admin/users/"+created.ID+"/keys/"+keys[0].ID, struct{}{}, 204)
	call("PUT", "/portal/api/admin/users/"+created.ID+"/overrides/rpm", map[string]any{"value": 3, "reason": "private-reason-content-must-not-enter-audit"}, 204)
	call("DELETE", "/portal/api/admin/users/"+created.ID+"/overrides/rpm", struct{}{}, 204)
	call("PUT", "/portal/api/admin/users/"+created.ID+"/groups", map[string]any{"groups": []string{"staff"}}, 204)
	call("PUT", "/portal/api/admin/users/"+created.ID+"/enabled", map[string]any{"enabled": false}, 204)
	call("PUT", "/portal/api/admin/users/"+created.ID+"/enabled", map[string]any{"enabled": true}, 204)
	call("POST", "/portal/api/admin/users/"+created.ID+"/password", map[string]string{"password": "history-new-temporary-password"}, 204)
	// Protected policy/credential edits are refused and tied to the actor UUID.
	call("PUT", "/portal/api/admin/users/"+root.ID+"/overrides/rpm", map[string]any{"value": 1}, 403)
	var page audit.Page
	_ = json.Unmarshal(call("GET", "/portal/api/admin/history?actor="+root.ID, nil, 200), &page)
	actions := []string{}
	for _, event := range page.Events {
		if event.Actor.ID != root.ID || event.Actor.Name != "" {
			t.Fatal("actor attribution", event)
		}
		actions = append(actions, event.Action+":"+event.Result)
	}
	for _, want := range []string{"user.create_local:succeeded", "key.revoke:succeeded", "user.override_set:succeeded", "user.override_clear:succeeded", "user.local_groups:succeeded", "user.enablement:succeeded", "credential.reset:succeeded", "user.override_set:refused"} {
		if !strings.Contains(strings.Join(actions, " "), want) {
			t.Fatal("missing event", want, actions)
		}
	}
	// Small pages remain disjoint and support stable target/action/result filters.
	_ = json.Unmarshal(call("GET", "/portal/api/admin/history?limit=2", nil, 200), &page)
	if len(page.Events) != 2 || page.NextBefore == 0 {
		t.Fatal("pagination", page)
	}
	previous := page.Events[1].ID
	_ = json.Unmarshal(call("GET", "/portal/api/admin/history?limit=2&before="+jsonNumber(page.NextBefore), nil, 200), &page)
	if len(page.Events) != 2 || page.Events[0].ID >= previous {
		t.Fatal("cursor repeated events")
	}
	for _, query := range []string{"limit=201", "before=-1", "result=unknown", "from=not-a-date", "from=2026-02-02T00:00:00Z&to=2026-01-01T00:00:00Z"} {
		call("GET", "/portal/api/admin/history?"+query, nil, 400)
	}
	if err = a.ChangeLocalPassword(t.Context(), created.ID, []byte("history-new-temporary-password"), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	ordinary := f.browser(t)
	ordinary.localLogin(t, "history-user", rotatedAdminPassword, 200)
	if resp, _ := ordinary.call(t, "GET", "/portal/api/admin/history", "", ""); resp.StatusCode != 403 {
		t.Fatal("ordinary history access", resp.StatusCode)
	}
	if err = a.DeleteUser(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	history, err := audit.List(t.Context(), f.pool, audit.Filter{TargetID: created.ID})
	if err != nil || len(history.Events) == 0 {
		t.Fatal("history lost on erasure", err)
	}
	encoded, _ := json.Marshal(history)
	var stored string
	if err = f.pool.QueryRow(t.Context(), `SELECT string_agg(row_to_json(e)::text,' ') FROM audit_events e`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{initialAdminPassword, rotatedAdminPassword, "history-new-temporary-password", key, "history-secret-label-not-retained", "private-reason-content-must-not-enter-audit", "$argon2id"} {
		if strings.Contains(stored+string(encoded)+f.logs.String(), secret) {
			t.Fatal("secret/content in history or log")
		}
	}
	// The real binary reads the identical history service and filters.
	binary := filepath.Join(t.TempDir(), "olr")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	cmd := exec.CommandContext(t.Context(), binary, "admin", "history", "-action", "user.delete", "-target", created.ID)
	cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+f.pool.Config().ConnString())
	output, err := cmd.CombinedOutput()
	if err != nil || json.Unmarshal(output, &page) != nil || len(page.Events) != 1 || page.Events[0].Action != "user.delete" {
		t.Fatalf("CLI history: %v %s", err, output)
	}
}

func TestAuditWriteFailureRollsBackSensitiveChanges(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	a := auth.Admin{Pool: pool}
	root, _, err := a.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ChangeLocalPassword(t.Context(), root.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	owner, err := a.CreateLocalUser(t.Context(), "audit-owner", "", []string{"staff"}, []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ChangeLocalPassword(t.Context(), owner, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	_, err = a.CreateKey(t.Context(), owner, "audit-key", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `CREATE FUNCTION reject_successful_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.result='succeeded' THEN RAISE EXCEPTION 'synthetic audit outage'; END IF; RETURN NEW; END; $$;
 CREATE TRIGGER reject_successful_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION reject_successful_audit()`)
	if err = a.ResetLocalPassword(t.Context(), owner, []byte("new-password-must-not-commit")); err == nil {
		t.Fatal("reset claimed audit success")
	}
	if _, err = a.VerifyLocalPassword(t.Context(), "audit-owner", []byte(rotatedAdminPassword)); err != nil {
		t.Fatal("reset committed without audit", err)
	}
	if err = a.SetEnabled(t.Context(), owner, false); err == nil {
		t.Fatal("disable committed without audit")
	}
	if err = a.DeleteUser(t.Context(), owner); err == nil {
		t.Fatal("deletion committed without audit")
	}
	if _, err = a.AddUser(t.Context(), "unrecorded-user", "", nil); err == nil {
		t.Fatal("creation committed without audit")
	}
	if _, err = a.RecoverProtectedAdmin(t.Context(), []byte("unrecorded-recovery-password")); err == nil {
		t.Fatal("recovery committed without audit")
	}
	if _, err = a.VerifyLocalPassword(t.Context(), "admin", []byte(rotatedAdminPassword)); err != nil {
		t.Fatal("recovery changed credential", err)
	}
	keys, err := a.ListKeys(t.Context(), owner)
	if err != nil || len(keys) != 1 || keys[0].Revoked {
		t.Fatal("rollback lost original key", err)
	}
	var enabled bool
	if err = pool.QueryRow(t.Context(), `SELECT enabled FROM users WHERE id=$1`, owner).Scan(&enabled); err != nil || !enabled {
		t.Fatal("rollback lost enabled user", err)
	}
	var count int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE username='unrecorded-user'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial user creation", err)
	}
	// Configuration mutations obey the same transaction rule.
	env := managedEnvironment("")
	cfg := managedConfig(t, env)
	service := settings.AuthenticationService{DB: pool, File: cfg, Lookup: func(name string) (string, bool) { v, ok := env[name]; return v, ok }}
	if _, err = service.Import(t.Context(), settings.DomainAuthentication, audit.OperatorCLI()); err == nil {
		t.Fatal("import committed without audit")
	}
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM settings_state`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial settings import", err)
	}
	// Application writes cannot rewrite/remove or truncate events, including
	// endpoint revisions; SQL owner/superuser access is outside this model.
	for _, statement := range []string{`UPDATE audit_events SET reason='rewritten'`, `DELETE FROM audit_events`, `TRUNCATE audit_events`} {
		if _, err = pool.Exec(t.Context(), statement); err == nil {
			t.Fatal("history was mutable", statement)
		}
	}
	page, err := audit.List(context.Background(), pool, audit.Filter{Result: audit.Failed})
	if err != nil || len(page.Events) < 5 {
		t.Fatal("failed attempts not retained", err)
	}
}
