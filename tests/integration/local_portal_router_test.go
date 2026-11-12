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
)

func TestLocalPortalBinaryStartsAndRestartsDuringOIDCOutage(t *testing.T) {
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
	configFile := filepath.Join(dir, "config.yaml")
	for _, name := range []string{"OIDC_ISSUER", "OIDC_BACKCHANNEL_URL", "OIDC_AUDIENCE", "OIDC_REQUIRED_CLAIM", "OIDC_SYNC_CLIENT_ID"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD", initialAdminPassword)
	t.Setenv("OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE", "")
	t.Setenv("PORTAL_BASE_URL", "http://portal.example.test")
	t.Setenv("PORTAL_COOKIE_SECURE", "false")
	t.Setenv("OIDC_PORTAL_CLIENT_SECRET", "test-secret")
	t.Setenv("OIDC_SYNC_CLIENT_SECRET", "private-sync-secret-for-test")
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	logs := &authLog{}
	password := initialAdminPassword
	for _, oidc := range []string{"", `oidc:
  issuer: http://127.0.0.1:1/realms/test
  portal_client_id: olr-portal
  audience: olr-api
`} {
		if err := os.WriteFile(configFile, fmt.Appendf(nil, `%s
models:
  test:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 4}
`, oidc, backend.URL), 0600); err != nil {
			t.Fatal(err)
		}
		base, stop := startRouter(t, binary, configFile, database.dsn, logs)
		probe(t, base, "/readyz", 200)
		call := func(method, path string, body any, want int) []byte {
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
		if oidc != "" {
			call("GET", "/portal/login", nil, 503)
			call("GET", "/portal/callback?state=bogus&code=bogus", nil, 503)
			// The previous local session also survives a server restart.
			call("GET", "/portal/api/admin/users", nil, 200)
		}
		call("POST", "/portal/login/local", map[string]string{"username": "admin", "password": password}, 200)
		if oidc == "" {
			call("GET", "/portal/api/admin/users", nil, 403)
			call("POST", "/portal/password", map[string]string{"current_password": initialAdminPassword, "new_password": rotatedAdminPassword}, 200)
			password = rotatedAdminPassword
			call("POST", "/portal/login/local", map[string]string{"username": "admin", "password": password}, 200)
		}
		call("GET", "/portal/api/admin/users", nil, 200)
		call("GET", "/portal/", nil, 200)
		methods := call("GET", "/portal/api/auth", nil, 200)
		if oidc != "" && !strings.Contains(string(methods), `"available":false`) {
			t.Fatal("SSO failure hidden from auth status")
		}
		stop()
	}
	for _, secret := range []string{initialAdminPassword, rotatedAdminPassword, "private-sync-secret-for-test", "test-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("startup logged credential")
		}
	}
}
