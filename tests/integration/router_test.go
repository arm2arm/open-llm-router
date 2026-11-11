package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRouterDatabaseLifecycle(t *testing.T) {
	database := newDatabase(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build router: %v\n%s", err, output)
	}
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
	// Refuse startup before listening when the database is missing, malformed,
	// or unavailable. Disabling only our private database leaves Compose intact.
	setConnections := func(allowed bool) {
		t.Helper()
		if _, err := database.admin.Exec(t.Context(), fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS %t", pgx.Identifier{database.name}.Sanitize(), allowed)); err != nil {
			t.Fatal(err)
		}
	}
	setConnections(false)
	for _, tc := range []struct{ name, dsn, message string }{
		{"missing", "", "OLR_DATABASE_URL is required"},
		{"malformed", "postgres://user:private-password@host:invalid/db", "not a valid Postgres connection string"},
		{"unavailable", database.dsn, "connect to database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "serve", "-config", config, "-listen", "127.0.0.1:0")
			cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+tc.dsn)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.message) || strings.Contains(string(output), `"listening"`) || strings.Contains(string(output), "private-password") {
				t.Fatalf("startup: err=%v output=%s", err, output)
			}
		})
	}
	setConnections(true)
	base, stop := startRouter(t, binary, config, database.dsn)
	probe(t, base, "/readyz", http.StatusOK)
	// Check that the real binary created the schema in a previously empty DB.
	conn, err := pgx.Connect(t.Context(), database.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var id string
	if err := conn.QueryRow(t.Context(), `INSERT INTO users (issuer, sub) VALUES ('local', 'restart') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	stop()
	base, _ = startRouter(t, binary, config, database.dsn)
	probe(t, base, "/readyz", http.StatusOK)
	var got string
	if err := conn.QueryRow(t.Context(), `SELECT id FROM users WHERE sub = 'restart'`).Scan(&got); err != nil || got != id {
		t.Fatalf("user after router restart = %q, %v", got, err)
	}
	if err := conn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	setConnections(false)
	if _, err := database.admin.Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, database.name); err != nil {
		t.Fatal(err)
	}
	probe(t, base, "/readyz", http.StatusServiceUnavailable)
	probe(t, base, "/healthz", http.StatusOK)
	setConnections(true)
	probe(t, base, "/readyz", http.StatusOK)
}

// Start the actual binary on an OS-assigned port and read its startup log to
// discover the address. Each process is stopped and reaped, even on failure.
func startRouter(t *testing.T, binary, config, dsn string, logOutput ...io.Writer) (string, func()) {
	t.Helper()
	cmd := exec.Command(binary, "serve", "-config", config, "-listen", "127.0.0.1:0", "-shutdown-grace", "1s")
	cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+dsn)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		t.Helper()
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Error("router exit:", err)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("router did not shut down")
			}
		})
	}
	t.Cleanup(stop)
	addresses := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			for _, output := range logOutput {
				fmt.Fprintln(output, scanner.Text())
			}
			var entry struct{ Msg, Addr string }
			if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.Msg == "listening" {
				addresses <- entry.Addr
			}
		}
		close(addresses)
	}()
	select {
	case address := <-addresses:
		if address == "" {
			t.Fatal("router exited before listening")
		}
		return "http://" + address, stop
	case <-time.After(15 * time.Second):
		t.Fatal("router did not start")
		return "", stop
	}
}

func probe(t *testing.T, base, path string, want int) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s = %d %s, want %d", path, resp.StatusCode, body, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
