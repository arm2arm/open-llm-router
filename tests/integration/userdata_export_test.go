package integration_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/arm2arm/open-llm-router/internal/userdata"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCLIUserExportAndDisabledAccountLifecycle(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	add := func(name string) string {
		t.Helper()
		id, err := admin.AddUser(t.Context(), name, name+"@example.org", []string{"staff"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alice, bob, forwarder := add("export-alice \"quoted\" \\ name"), add("export-bob"), add("export-forwarder")
	secret, err := admin.CreateKey(t.Context(), alice, "personal fixture", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, pool, `INSERT INTO usage(user_id,forwarded_for,model,replica,protocol,status,input_tokens,output_tokens,latency_ms,request_id) SELECT $1,$2,'for-alice','replica','openai','200',10,20,50,'forwarded-'||n::text FROM generate_series(1,203) n`, forwarder, alice)
	for _, row := range []struct {
		owner      string
		attributed any
		model      string
	}{{alice, nil, "alice-direct"}, {forwarder, nil, "forwarder-direct"}, {forwarder, bob, "for-bob"}} {
		execSQL(t, pool, `INSERT INTO usage(user_id,forwarded_for,model,replica,protocol,status,latency_ms) VALUES($1,$2,$3,'replica','openai','200',10)`, row.owner, row.attributed, row.model)
	}
	execSQL(t, pool, `INSERT INTO usage_daily(day,user_id,model,requests,input_tokens,output_tokens) VALUES(current_date,$1,'for-alice',203,2030,4060)`, alice)
	execSQL(t, pool, `INSERT INTO sessions(id_hash,user_id,expires) VALUES(decode(repeat('ab',32),'hex'),$1,now()+interval '1 hour')`, alice)
	execSQL(t, pool, `INSERT INTO request_traces(request_id,user_id,started_at,expires_at,detail) VALUES('export-trace',$1,now(),now()+interval '1 day','{"v":2,"input":[{"role":"user","blocks":[{"type":"text","text":"Bearer export-private-secret"}]}]}')`, alice)
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build export CLI: %v\n%s", err, output)
	}
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(t.Context(), binary, append([]string{"admin", "user"}, args...)...)
		cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn)
		return cmd
	}
	file := filepath.Join(dir, "alice.json")
	if output, err := command("export", "-id", alice, "-output", file).CombinedOutput(); err != nil {
		t.Fatalf("export failed: %v %s", err, output)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("export file permissions", err)
	}
	var exported struct {
		User        auth.UserInfo
		Keys        []map[string]any
		Usage       []struct{ Model string }
		DailyTotals []any `json:"daily_totals"`
		Sessions    []map[string]any
		History     []any `json:"administration_history"`
		Traces      []any `json:"request_traces"`
	}
	if json.Unmarshal(data, &exported) != nil || exported.User.ID != alice || len(exported.Keys) != 1 || len(exported.Usage) != 204 || len(exported.DailyTotals) != 1 || len(exported.Sessions) != 1 || len(exported.History) == 0 || len(exported.Traces) != 1 {
		t.Fatal("incomplete user export")
	}
	for _, private := range []string{secret, `"hash"`, `"id_hash"`, `"password_hash"`, "export-private-secret", "for-bob", "forwarder-direct"} {
		if bytes.Contains(data, []byte(private)) {
			t.Fatal("export leaked credentials or another attributed user's request")
		}
	}
	if _, err := command("export", "-id", alice, "-output", file).CombinedOutput(); err == nil {
		t.Fatal("export overwrote an existing file")
	}
	data, err = command("export", "-id", forwarder).Output()
	if err != nil || json.Unmarshal(data, &exported) != nil || len(exported.Usage) != 1 || exported.Usage[0].Model != "forwarder-direct" {
		t.Fatal("forwarder export included end-user requests", err)
	}
	// Failed exports remove the newly created partial file.
	missing := filepath.Join(dir, "missing.json")
	if _, err := command("export", "-id", "00000000-0000-0000-0000-000000000001", "-output", missing).CombinedOutput(); err == nil {
		t.Fatal("missing user export succeeded")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("failed export left a partial file")
	}
	old, fresh := add("old-departed"), add("fresh-departed")
	for _, id := range []string{old, fresh} {
		if err := admin.DisableUser(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, pool, `UPDATE users SET disabled_at=now()-interval '100 days' WHERE id=$1`, old)
	// Disable again must retain the original start of the disabled period.
	if err := admin.DisableUser(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	data, err = command("list", "-disabled", "-older-than", "2160h").Output()
	var candidates []auth.UserInfo
	if err != nil || json.Unmarshal(data, &candidates) != nil || len(candidates) != 1 || candidates[0].ID != old || candidates[0].DisabledAt == nil {
		t.Fatal("disabled-age list returned wrong candidates", err)
	}
	// Existing credentials and usage must cascade when an operator removes a candidate.
	execSQL(t, pool, `INSERT INTO api_keys(user_id,prefix,hash,label) VALUES($1,'old-test',decode(repeat('cd',32),'hex'),'old key')`, old)
	execSQL(t, pool, `INSERT INTO usage(user_id,model,replica,protocol,status,latency_ms) VALUES($1,'old','replica','openai','200',10)`, old)
	if output, err := command("delete", "-id", old).CombinedOutput(); err != nil {
		t.Fatalf("delete candidate: %v %s", err, output)
	}
	var remaining int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users WHERE id=$1)+(SELECT count(*) FROM api_keys WHERE user_id=$1)+(SELECT count(*) FROM usage WHERE user_id=$1)`, old).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("old disabled user's owned data remained", remaining, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE id=$1 AND NOT enabled`, fresh).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatal("fresh disabled user was removed", remaining, err)
	}
	if err := admin.SetEnabled(t.Context(), fresh, true); err != nil {
		t.Fatal(err)
	}
	var reset bool
	if err := pool.QueryRow(t.Context(), `SELECT disabled_at IS NULL FROM users WHERE id=$1`, fresh).Scan(&reset); err != nil || !reset {
		t.Fatal("re-enabled identity retained disabled age", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("list revealed a credential")
	}
}

func TestDisabledAgeUpgradeDoesNotInventHistoricalDisableDate(t *testing.T) {
	database := newDatabase(t)
	legacy, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	execSQL(t, legacy, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations()[:27] {
		execSQL(t, legacy, migration.SQL)
		execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	id := user(t, legacy, "local", "old-disabled-age")
	execSQL(t, legacy, `UPDATE users SET enabled=false,synced_at=now()-interval '5 years' WHERE id=$1`, id)
	before := time.Now().Add(-time.Second)
	upgraded := openDatabase(t, database.dsn)
	var disabled time.Time
	if err := upgraded.QueryRow(t.Context(), `SELECT disabled_at FROM users WHERE id=$1`, id).Scan(&disabled); err != nil || disabled.Before(before) {
		t.Fatal("upgrade invented an old disabled timestamp", err)
	}
}

func TestUserExportSnapshotDoesNotIncludeLaterRequests(t *testing.T) {
	pool := openDatabase(t, newDatabase(t).dsn)
	id := user(t, pool, "local", "snapshot-export")
	var output bytes.Buffer
	writer := &exportMutationWriter{Buffer: &output, mutate: func() {
		execSQL(t, pool, `INSERT INTO usage(user_id,model,replica,protocol,status,latency_ms) VALUES($1,'after-snapshot','r','openai','200',10)`, id)
	}}
	if err := userdata.Export(t.Context(), pool, id, writer); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("after-snapshot")) {
		t.Fatal("export was not a consistent snapshot")
	}
}

type exportMutationWriter struct {
	*bytes.Buffer
	mutate func()
}

func (w *exportMutationWriter) Write(p []byte) (int, error) {
	if w.mutate != nil {
		f := w.mutate
		w.mutate = nil
		f()
	}
	return w.Buffer.Write(p)
}
func (w *exportMutationWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }
