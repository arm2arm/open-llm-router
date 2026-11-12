package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestProtectedAdminCLIAndStartupUpgrade(t *testing.T) {
	database := newDatabase(t)
	// Start with the previous deployed schema and an unrelated SSO "admin".
	legacy, err := pgxpool.New(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(legacy.Close)
	execSQL(t, legacy, `CREATE TABLE schema_migrations(version integer PRIMARY KEY,name text NOT NULL,applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`)
	for _, migration := range db.Migrations() {
		if migration.Version >= 4 {
			break
		}
		execSQL(t, legacy, migration.SQL)
		execSQL(t, legacy, `INSERT INTO schema_migrations(version,name) VALUES($1,$2)`, migration.Version, migration.Name)
	}
	execSQL(t, legacy, `INSERT INTO users(issuer,sub,username) VALUES('https://idp.example/realms/test','admin','admin')`)

	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build router: %v\n%s", err, output)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, fmt.Appendf(nil, `models:
  test:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 4}
`, backend.URL), 0600); err != nil {
		t.Fatal(err)
	}
	initialFile, nextFile := filepath.Join(dir, "initial"), filepath.Join(dir, "replacement")
	for file, password := range map[string]string{initialFile: initialAdminPassword, nextFile: rotatedAdminPassword} {
		if err := os.WriteFile(file, []byte(password+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Bad bootstrap sources stop startup before listening, with no secret in
	// the error, and cannot leave a partially initialized administrator.
	for _, tc := range []struct {
		name, password, file, message string
	}{
		{"missing file", "", initialFile + "-missing", "password file could not be opened"},
		{"ambiguous sources", initialAdminPassword, initialFile, "only one bootstrap"},
		{"weak password", "short", "", "at least 15 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "serve", "-config", config, "-listen", "127.0.0.1:0")
			cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn,
				"OLR_BOOTSTRAP_ADMIN_PASSWORD="+tc.password, "OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE="+tc.file)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.message) || strings.Contains(string(output), `"listening"`) ||
				strings.Contains(string(output), initialAdminPassword) || strings.Contains(string(output), initialFile) {
				t.Fatal("unsafe bootstrap startup failure", err)
			}
		})
	}
	var partial int
	if err := legacy.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE protected_admin`).Scan(&partial); err != nil || partial != 0 {
		t.Fatal("invalid bootstrap left a partial administrator", err)
	}
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD", "")
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE", initialFile)
	logs := &authLog{}
	base, stop := startRouter(t, binary, config, database.dsn, logs)
	probe(t, base, "/readyz", 200)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	account, err := admin.ProtectedLocalAdmin(t.Context())
	if err != nil || !account.MustChangePassword {
		t.Fatal("startup did not bootstrap", err)
	}
	var identityCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM users WHERE username='admin'`).Scan(&identityCount); err != nil || identityCount != 2 {
		t.Fatal("upgrade linked an SSO account by username", err)
	}
	cli := func(wantError string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"admin"}, args...)...)
		cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn)
		output, err := cmd.CombinedOutput()
		if wantError == "" && err != nil || wantError != "" && (err == nil || !strings.Contains(string(output), wantError)) {
			t.Fatalf("admin %s: unexpected result: %v %s", args[0], err, output)
		}
		for _, secret := range []string{initialAdminPassword, rotatedAdminPassword, "$argon2"} {
			if strings.Contains(string(output), secret) {
				t.Fatal("CLI exposed a password or hash")
			}
		}
		return string(output)
	}
	cli("password change required", "key", "create", "-user", account.ID)
	for _, action := range []string{"delete", "disable"} {
		cli("protected local administrator", "user", action, "-id", account.ID)
	}
	cli("", "password", "change", "-user", account.ID, "-current-password-file", initialFile, "-password-file", nextFile)
	if _, err := admin.VerifyLocalPassword(t.Context(), "admin", []byte(initialAdminPassword)); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatal("CLI retained the bootstrap password", err)
	}
	verified, err := admin.VerifyLocalPassword(t.Context(), "admin", []byte(rotatedAdminPassword))
	if err != nil || verified.MustChangePassword || verified.ID != account.ID {
		t.Fatal("CLI did not rotate the credential", err)
	}
	var before, after string
	if err := pool.QueryRow(t.Context(), `SELECT password_hash FROM local_credentials WHERE user_id=$1`, account.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	stop()
	// Retire the secret file. Restart and explicit bootstrap must ignore it.
	if err := os.Remove(initialFile); err != nil {
		t.Fatal(err)
	}
	base, stop = startRouter(t, binary, config, database.dsn, logs)
	probe(t, base, "/readyz", 200)
	var result struct {
		auth.LocalAccount
		Created bool `json:"created"`
	}
	if err := json.Unmarshal([]byte(cli("", "bootstrap")), &result); err != nil || result.Created || result.ID != account.ID || result.MustChangePassword {
		t.Fatal("repeated CLI bootstrap changed the account", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT password_hash FROM local_credentials WHERE user_id=$1`, account.ID).Scan(&after); err != nil || before != after {
		t.Fatal("restart reset the credential", err)
	}
	stop()
	for _, secret := range []string{initialAdminPassword, rotatedAdminPassword, before, "$argon2"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("router logs exposed a password or hash")
		}
	}
}
