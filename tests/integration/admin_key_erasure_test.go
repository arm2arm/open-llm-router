package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/auth"
)

func TestCLIUserErasureRemovesAdministrationKeysAndDelegatedPreviews(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	owner, err := admin.AddUser(t.Context(), "erased-admin-owner", "", []string{"admins"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := admin.AddUser(t.Context(), "retained-admin-owner", "", []string{"admins"})
	if err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: "portal_user", ID: owner}
	issue := func(user string, parent *adminaccess.Key) adminaccess.Key {
		t.Helper()
		key, _, err := adminaccess.Issue(t.Context(), pool, user, adminaccess.Create{Label: "erasure fixture", Permissions: adminaccess.Permissions, Networks: []string{"127.0.0.1/32"}, Days: 1}, parent, 10, actor)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	active, revoked, expired := issue(owner, nil), issue(owner, nil), issue(owner, nil)
	child := issue(owner, &active)
	grandchild := issue(owner, &child)
	retained := issue(other, nil)
	execSQL(t, pool, `UPDATE admin_api_keys SET revoked=true WHERE id=$1`, revoked.ID)
	execSQL(t, pool, `UPDATE admin_api_keys SET expires=now()-interval '1 hour' WHERE id=$1`, expired.ID)
	for _, key := range []adminaccess.Key{active, revoked, expired, child, grandchild, retained} {
		var change string
		if err := pool.QueryRow(t.Context(), `INSERT INTO admin_changes(key_id,operation,arguments_hash,state_version,policy_version,summary) VALUES($1,'set_user_override',decode(repeat('00',32),'hex'),1,1,'{}') RETURNING id::text`, key.ID).Scan(&change); err != nil {
			t.Fatal(err)
		}
		if err := audit.Record(t.Context(), pool, audit.Event{Action: "agent.preview", Actor: actor, TargetType: "admin_change", TargetID: change, Result: audit.Succeeded}); err != nil {
			t.Fatal(err)
		}
	}
	var beforeEvents int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action LIKE 'admin_key.%' OR action LIKE 'agent.%'`).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build erasure CLI: %v\n%s", err, output)
	}
	configFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configFile, []byte("models:\n  test:\n    protocols: [openai]\n    timeout_s: 5\n    pool:\n      - {url: 'http://127.0.0.1:8000', weight: 1, max_inflight: 4}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "admin", "user", "delete", "-id", owner)
	command.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn, "OLR_CONFIG="+configFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("erase administration-key owner: %v\n%s", err, output)
	}
	var owners, keys, changes, events int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users WHERE id=$1),(SELECT count(*) FROM admin_api_keys),(SELECT count(*) FROM admin_changes),(SELECT count(*) FROM audit_events WHERE action LIKE 'admin_key.%' OR action LIKE 'agent.%')`, owner).Scan(&owners, &keys, &changes, &events); err != nil {
		t.Fatal(err)
	}
	if owners != 0 || keys != 1 || changes != 1 || events != beforeEvents {
		t.Fatal("erasure left owned keys/previews or removed immutable audit records", owners, keys, changes, events, beforeEvents)
	}
	var retainedID string
	if err := pool.QueryRow(t.Context(), `SELECT id::text FROM admin_api_keys`).Scan(&retainedID); err != nil || retainedID != retained.ID {
		t.Fatal("unrelated administration credential removed", retainedID, err)
	}
}
