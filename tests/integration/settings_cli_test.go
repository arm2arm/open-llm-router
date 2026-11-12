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

	"github.com/arm2arm/open-llm-router/internal/settings"
)

// The operator workflow through the real binary: import, a draft with a
// stored secret, activation, and router restarts. A wrong encryption key turns
// SSO off at startup while local administration keeps working.
func TestManagedSettingsCLIAndRouterStartup(t *testing.T) {
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
	for _, name := range []string{"OIDC_ISSUER", "OIDC_BACKCHANNEL_URL", "OIDC_AUDIENCE", "OIDC_REQUIRED_CLAIM", "OIDC_SYNC_CLIENT_ID", "OIDC_SYNC_CLIENT_SECRET",
		"OIDC_PORTAL_CLIENT_ID", "OIDC_GROUPS_CLAIM", "OIDC_USERNAME_CLAIM"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	const envSecret, storedSecret = "environment-portal-secret-value", "stored-sync-secret-value"
	key, wrongKey := settingsKey(3), settingsKey(4)
	t.Setenv("OIDC_PORTAL_CLIENT_SECRET", envSecret)
	t.Setenv(settings.EnvEncryptionKey, key)
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD", initialAdminPassword)
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE", "")
	t.Setenv("PORTAL_BASE_URL", "http://portal.example.test")
	t.Setenv("PORTAL_COOKIE_SECURE", "false")
	configFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configFile, fmt.Appendf(nil, `oidc:
  issuer: http://127.0.0.1:1/realms/unreachable
  portal_client_id: olr-portal
models:
  test:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 4}
`, backend.URL), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OLR_CONFIG", configFile)
	secretFile := filepath.Join(dir, "sync-secret")
	if err := os.WriteFile(secretFile, []byte(storedSecret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	secrets := []string{envSecret, storedSecret, key, wrongKey, initialAdminPassword}
	cli := func(wantError string, args ...string) (settings.AuthenticationView, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"admin", "settings"}, args...)...)
		cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if wantError == "" && err != nil || wantError != "" && (err == nil || !strings.Contains(stderr.String(), wantError)) {
			t.Fatalf("settings %s: %v %s %s", args[0], err, &stdout, &stderr)
		}
		for _, secret := range secrets {
			if strings.Contains(stdout.String()+stderr.String(), secret) {
				t.Fatal("settings CLI exposed a secret")
			}
		}
		var view settings.AuthenticationView
		if wantError == "" {
			if err := json.Unmarshal([]byte(stdout.String()), &view); err != nil {
				t.Fatal(err, stdout.String())
			}
		}
		return view, stderr.String()
	}

	view, _ := cli("", "show")
	if view.Managed {
		t.Fatal("fresh installation must be file-managed")
	}
	emptyDocument := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyDocument, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	cli("file-managed", "draft", "-expect-version", "1", "-document", emptyDocument)
	view, note := cli("", "import")
	if !view.Managed || !strings.Contains(note, fmt.Sprintf("revision %d is active", view.Active.ID)) {
		t.Fatal("import", note)
	}
	cli("already managed", "import")
	doc := *view.Active.Document
	doc.SyncIntervalS = 120
	documentFile := filepath.Join(dir, "draft.json")
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(documentFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	view, _ = cli("", "draft", "-expect-version", fmt.Sprint(view.Version), "-document", documentFile, "-secret", "sync_client_secret="+secretFile)
	if view.Draft == nil || !view.Draft.Validation.Valid || view.Draft.Document.SyncClientSecret.Stored == "" {
		t.Fatal("draft", view.Draft)
	}
	cli("changed since you loaded them", "activate", "-expect-version", fmt.Sprint(view.Version-1), "-revision", fmt.Sprint(view.Draft.ID))
	draft := view.Draft.ID
	view, _ = cli("", "activate", "-expect-version", fmt.Sprint(view.Version), "-revision", fmt.Sprint(draft))
	if view.Active.ID != draft {
		t.Fatal("activate")
	}

	// Routing is a second domain: import it from the file, then restart.
	cli("", "import", "-domain", "routing")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	logs := &authLog{}
	start := func() string {
		t.Helper()
		base, stop := startRouter(t, binary, configFile, database.dsn, logs)
		probe(t, base, "/readyz", 200)
		call := func(method, path string, body any, want int) []byte {
			t.Helper()
			encoded, _ := json.Marshal(body)
			req, _ := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(string(encoded)))
			req.Host = "portal.example.test"
			if method != "GET" {
				req.Header.Set("Origin", "http://portal.example.test")
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != want {
				t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
			}
			return data
		}
		// Local login works whatever happens to managed SSO settings.
		call("POST", "/portal/login/local", map[string]string{"username": "admin", "password": initialAdminPassword}, 200)
		methods := call("GET", "/portal/api/auth", nil, 200)
		stop()
		return string(methods)
	}
	authMethods := start()
	if !strings.Contains(logs.String(), `"msg":"managed authentication settings applied"`) || !strings.Contains(logs.String(), `"msg":"managed routing settings applied"`) || !strings.Contains(logs.String(), fmt.Sprintf(`"revision":%d`, draft)) ||
		!strings.Contains(authMethods, `"configured":true`) {
		t.Fatal("router did not start with the active revision", logs.String())
	}
	// Restart with a different key: the stored sync secret cannot be opened.
	t.Setenv(settings.EnvEncryptionKey, wrongKey)
	authMethods = start()
	if !strings.Contains(logs.String(), "SSO is off and local login remains available") || !strings.Contains(authMethods, `"configured":false`) {
		t.Fatal("wrong key did not turn SSO off safely", logs.String())
	}
	view, _ = cli("", "show")
	for _, setting := range view.Effective {
		if setting.Name == "sync_client_secret" && (setting.Secret == nil || !setting.Secret.Configured || setting.Secret.Available) {
			t.Fatal("secret state must report configured but unavailable", setting.Secret)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("router logs exposed a secret")
		}
	}
}
