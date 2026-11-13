package integration_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func TestRouterStartsWithoutAvailableModels(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); err != nil {
		t.Fatalf("build router: %v\n%s", err, output)
	}
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_catalog=%t", empty), func(t *testing.T) {
			database := newDatabase(t)
			// Reserve an unused origin, then close it: the sole configured model
			// initially has a connection-refused backend, not a healthy mock.
			backend := httptest.NewServer(mockllm.New(mockllm.Options{Tokens: 1}))
			origin, address := backend.URL, backend.Listener.Addr().String()
			backend.Close()
			config := filepath.Join(t.TempDir(), "config.yaml")
			data := "models: {}\ngroups: {staff: {models: ['*']}}\n"
			if !empty {
				data = fmt.Sprintf(`models:
  only-model:
    upstream_model: chat-model
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 1}
groups: {staff: {models: ['*']}}
`, origin)
			}
			if err := os.WriteFile(config, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			base, _ := startRouter(t, binary, config, database.dsn)
			probe(t, base, "/healthz", http.StatusOK)
			probe(t, base, "/readyz", http.StatusOK)
			probe(t, base, "/portal/login/local", http.StatusOK)
			pool := openDatabase(t, database.dsn)
			admin := auth.Admin{Pool: pool}
			user, err := admin.AddUser(t.Context(), "outage-test", "", []string{"staff"})
			if err != nil {
				t.Fatal(err)
			}
			key, err := admin.CreateKey(t.Context(), user, "outage-test", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Timeout: 3 * time.Second}
			call := func(method, path, body string) int {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return resp.StatusCode
			}
			if code := call("GET", "/v1/models", ""); code != http.StatusOK {
				t.Fatal("model catalog inaccessible during outage", code)
			}
			if empty {
				return
			}
			body := `{"model":"only-model","messages":[],"max_tokens":1}`
			if code := call("POST", "/v1/chat/completions", body); code != http.StatusServiceUnavailable {
				t.Fatal("unavailable inference did not return 503", code)
			}
			// Bring the backend up at the same origin. Health checking must recover
			// inference without restarting the router or losing portal readiness.
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			recovered := httptest.NewUnstartedServer(mockllm.New(mockllm.Options{Tokens: 1}))
			recovered.Listener.Close()
			recovered.Listener = listener
			recovered.Start()
			defer recovered.Close()
			deadline := time.Now().Add(10 * time.Second)
			for code := call("POST", "/v1/chat/completions", body); code != http.StatusOK; code = call("POST", "/v1/chat/completions", body) {
				if time.Now().After(deadline) {
					t.Fatal("inference did not recover after backend startup", code)
				}
				time.Sleep(50 * time.Millisecond)
			}
			probe(t, base, "/readyz", http.StatusOK)
		})
	}
}
