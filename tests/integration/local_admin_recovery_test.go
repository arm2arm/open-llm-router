package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
)

const recoveryAdminPassword = "temporary private recovery passphrase"
const recoveredAdminPassword = "final private admin passphrase after recovery"

// Operator recovery through the real binary while the configured Keycloak is
// unreachable: the old password, sessions and API keys stop working, the
// temporary password must be replaced, and neither restart nor the retired
// bootstrap secret undoes the recovery.
func TestProtectedAdminRecoveryCLIDuringIdPOutage(t *testing.T) {
	database := newDatabase(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	for _, name := range []string{"OIDC_ISSUER", "OIDC_BACKCHANNEL_URL", "OIDC_AUDIENCE", "OIDC_REQUIRED_CLAIM", "OIDC_SYNC_CLIENT_ID"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{}
	for name, password := range map[string]string{"bootstrap": initialAdminPassword, "rotated": rotatedAdminPassword,
		"recovery": recoveryAdminPassword, "final": recoveredAdminPassword} {
		files[name] = filepath.Join(dir, name)
		if err := os.WriteFile(files[name], []byte(password+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	secrets := []string{initialAdminPassword, rotatedAdminPassword, recoveryAdminPassword, recoveredAdminPassword, "$argon2"}
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD", "")
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE", files["bootstrap"])
	t.Setenv("PORTAL_BASE_URL", "http://portal.example.test")
	t.Setenv("PORTAL_COOKIE_SECURE", "false")
	t.Setenv("OIDC_PORTAL_CLIENT_SECRET", "test-secret")
	configFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configFile, fmt.Appendf(nil, `oidc:
  issuer: http://127.0.0.1:1/realms/unreachable
  portal_client_id: olr-portal
  audience: olr-api
groups:
  staff: {models: ["*"]}
models:
  test:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 4}
`, backend.URL), 0600); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		auth.Recovery
		stdout, stderr string
	}
	// cli runs an admin command with only the database URL: no OIDC settings
	// are needed. The bootstrap environment stays set to prove recovery never
	// reads it. Output must never contain a password or hash.
	cli := func(stdin, wantError string, args ...string) outcome {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"admin"}, args...)...)
		cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn, "OLR_BOOTSTRAP_ADMIN_PASSWORD="+initialAdminPassword)
		cmd.Env = append(cmd.Env, "OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE=")
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if wantError == "" && err != nil || wantError != "" && (err == nil || !strings.Contains(stderr.String(), wantError)) {
			t.Fatalf("admin %s: unexpected result: %v %s %s", strings.Join(args, " "), err, &stdout, &stderr)
		}
		for _, secret := range append(secrets, files["recovery"], files["final"]) {
			if strings.Contains(stdout.String()+stderr.String(), secret) {
				t.Fatal("admin command output exposed a credential or secret path")
			}
		}
		for _, secret := range secrets {
			if strings.Contains(strings.Join(cmd.Args, " "), secret) {
				t.Fatal("a credential was passed in argv")
			}
		}
		var result outcome
		result.stdout, result.stderr = stdout.String(), stderr.String()
		if wantError == "" && args[0] == "recover" {
			if err := json.Unmarshal([]byte(stdout.String()), &result.Recovery); err != nil {
				t.Fatal("recovery output", err, stdout.String())
			}
		}
		return result
	}
	auditEvent := func(t *testing.T, stderr string) map[string]any {
		t.Helper()
		for line := range strings.SplitSeq(stderr, "\n") {
			var entry struct {
				Msg   string         `json:"msg"`
				Event map[string]any `json:"event"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "admin audit" {
				return entry.Event
			}
		}
		t.Fatalf("no audit event in %q", stderr)
		return nil
	}

	// Recovery cannot create the administrator.
	refused := cli(recoveryAdminPassword, "not bootstrapped", "recover", "-password-stdin")
	if event := auditEvent(t, refused.stderr); event["result"] != "refused" || event["action"] != "local_admin.recover" {
		t.Fatal("unbootstrapped recovery audit", event)
	}

	logs := &authLog{}
	base, stop := startRouter(t, binary, configFile, database.dsn, logs)
	probe(t, base, "/readyz", 200)
	pool := openDatabase(t, database.dsn)
	admin := auth.Admin{Pool: pool}
	account, err := admin.ProtectedLocalAdmin(t.Context())
	if err != nil {
		t.Fatal("startup did not bootstrap", err)
	}
	cli("", "", "password", "change", "-user", account.ID, "-current-password-file", files["bootstrap"], "-password-file", files["rotated"])

	type browser struct{ client *http.Client }
	newBrowser := func() browser {
		jar, _ := cookiejar.New(nil)
		return browser{&http.Client{Jar: jar, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	call := func(b browser, method, path string, body any, header http.Header, want int) []byte {
		t.Helper()
		encoded := ""
		if body != nil {
			data, _ := json.Marshal(body)
			encoded = string(data)
		}
		req, _ := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(encoded))
		req.Host = "portal.example.test"
		if method != "GET" {
			req.Header.Set("Origin", "http://portal.example.test")
			req.Header.Set("Content-Type", "application/json")
		}
		for name, values := range header {
			req.Header[name] = values
		}
		resp, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, data)
		}
		return data
	}
	login := func(b browser, password string, want int) {
		t.Helper()
		call(b, "POST", "/portal/login/local", map[string]string{"username": "admin", "password": password}, nil, want)
	}

	// An established admin session and API key before recovery.
	old := newBrowser()
	login(old, rotatedAdminPassword, 200)
	call(old, "GET", "/portal/api/admin/users", nil, nil, 200)
	key := cli("", "", "key", "create", "-user", account.ID, "-label", "pre-recovery").stdout
	bearer := http.Header{"Authorization": {"Bearer " + strings.TrimSpace(key)}}
	call(newBrowser(), "GET", "/v1/models", nil, bearer, 200)

	// Recover with only the database; Keycloak is configured but down.
	call(newBrowser(), "GET", "/portal/login", nil, nil, 503)
	recovered := cli(recoveryAdminPassword+"\n", "", "recover", "-password-stdin")
	var recordedRevision, credentialRevision int64
	if err := pool.QueryRow(t.Context(), `SELECT revision FROM local_credentials WHERE user_id=$1`, account.ID).Scan(&credentialRevision); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT revision FROM audit_events WHERE action='local_admin.recover' AND target_id=$1 AND result='succeeded' ORDER BY id DESC LIMIT 1`, account.ID).Scan(&recordedRevision); err != nil || recordedRevision != credentialRevision {
		t.Fatal("CLI recovery lacks transactional history", err)
	}

	if recovered.ID != account.ID || !recovered.ProtectedAdmin || !recovered.MustChangePassword || !recovered.CredentialReplaced ||
		recovered.SessionsRevoked < 1 || recovered.KeysRevoked != 1 {
		t.Fatalf("recovery result %+v", recovered.Recovery)
	}
	event := auditEvent(t, recovered.stderr)
	if event["result"] != "succeeded" || event["target_id"] != account.ID || event["actor"].(map[string]any)["type"] != "operator_cli" {
		t.Fatal("recovery audit event", event)
	}

	// Old password, session and key no longer work.
	call(old, "GET", "/portal/api/admin/users", nil, nil, 401)
	login(newBrowser(), rotatedAdminPassword, 401)
	call(newBrowser(), "GET", "/v1/models", nil, bearer, 401)
	// The temporary password only permits the required change.
	pending := newBrowser()
	login(pending, recoveryAdminPassword, 200)
	call(pending, "GET", "/portal/api/admin/users", nil, nil, 403)

	// A retry (e.g. after an unacknowledged invalidation) is idempotent and
	// ends the pending session too; it does not rehash or advance the revision.
	retried := cli(recoveryAdminPassword, "", "recover", "-password-file", files["recovery"])
	revision := func(o outcome) any { return auditEvent(t, o.stderr)["details"].(map[string]any)["credential_revision"] }
	if retried.CredentialReplaced || retried.SessionsRevoked != 1 || retried.KeysRevoked != 0 || retried.ID != account.ID ||
		revision(retried) != revision(recovered) || revision(recovered) == nil {
		t.Fatalf("retry result %+v", retried.Recovery)
	}
	call(pending, "GET", "/portal/api/auth/session", nil, nil, 401)

	// Complete the required change in the portal.
	pending = newBrowser()
	login(pending, recoveryAdminPassword, 200)
	call(pending, "POST", "/portal/password", map[string]string{"current_password": recoveryAdminPassword, "new_password": recoveredAdminPassword}, nil, 200)
	login(pending, recoveredAdminPassword, 200)
	call(pending, "GET", "/portal/api/admin/users", nil, nil, 200)

	// The current permanent password cannot be "recovered" to itself, and a
	// weak temporary password is refused, both with an audit record.
	reuse := auditEvent(t, cli(recoveredAdminPassword, "must differ", "recover", "-password-stdin").stderr)
	weak := auditEvent(t, cli("too short", "at least 15 characters", "recover", "-password-stdin").stderr)
	if reuse["result"] != "refused" || reuse["target_id"] != account.ID || weak["result"] != "refused" {
		t.Fatal("refused recovery audit", reuse, weak)
	}
	var hash string
	if err := pool.QueryRow(t.Context(), `SELECT password_hash FROM local_credentials WHERE user_id=$1`, account.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	stop()

	// Restart with the retired bootstrap secret still configured, then try an
	// explicit bootstrap with it: neither resets the recovered credential.
	base, stop = startRouter(t, binary, configFile, database.dsn, logs)
	probe(t, base, "/readyz", 200)
	bootstrap := exec.Command(binary, "admin", "bootstrap", "-password-file", files["bootstrap"])
	bootstrap.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn)
	if output, err := bootstrap.Output(); err != nil || !strings.Contains(string(output), `"created":false`) || !strings.Contains(string(output), account.ID) {
		t.Fatal("bootstrap after recovery", err, string(output))
	}
	var after string
	var protected bool
	if err := pool.QueryRow(t.Context(), `SELECT c.password_hash,u.protected_admin FROM local_credentials c JOIN users u ON u.id=c.user_id
		WHERE u.id=$1 AND u.enabled AND u.username='admin'`, account.ID).Scan(&after, &protected); err != nil || after != hash || !protected {
		t.Fatal("restart or bootstrap changed the recovered administrator", err)
	}
	login(newBrowser(), initialAdminPassword, 401)
	login(newBrowser(), recoveryAdminPassword, 401)
	login(newBrowser(), recoveredAdminPassword, 200)
	stop()
	for _, secret := range append(secrets, hash) {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("router logs exposed a credential")
		}
	}
}
