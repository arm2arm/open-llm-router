package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

type authLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *authLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}

func (b *authLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestAPIKeysCLI(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../../cmd/olr")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build router: %v\n%s", err, output)
	}
	backend := httptest.NewServer(mockllm.New(mockllm.Options{}))
	defer backend.Close()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, fmt.Appendf(nil, `auth: {default_key_lifetime_days: 7}
models:
  chat-model:
    protocols: [openai, anthropic]
    timeout_s: 5
    pool:
      - {url: %s, weight: 1, max_inflight: 4}
groups:
  staff: {models: [chat-model]}
`, backend.URL), 0600); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"admin"}, args...)...)
		cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+database.dsn, "OLR_CONFIG="+config)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("admin %s: %v stderr=%s", args[0], err, &stderr)
		}
		if stderr.Len() != 0 {
			t.Fatal("unexpected admin stderr:", &stderr)
		}
		return strings.TrimSpace(stdout.String())
	}
	owner := cli("user", "add", "-username", "pilot", "-email", "pilot@example.org", "-groups", "staff,admins")
	var issuer, username string
	var groups []string
	if err := pool.QueryRow(t.Context(), `SELECT issuer, username, groups FROM users WHERE id = $1`, owner).
		Scan(&issuer, &username, &groups); err != nil || issuer != "local" || username != "pilot" || len(groups) != 2 {
		t.Fatalf("bootstrap user: issuer=%s username=%s groups=%v err=%v", issuer, username, groups, err)
	}
	key := cli("key", "create", "-user", owner, "-label", "laptop")
	var keys []auth.KeyInfo
	listed := cli("key", "list", "-user", owner)
	if err := json.Unmarshal([]byte(listed), &keys); err != nil || len(keys) != 1 {
		t.Fatalf("key list: %s, %v", listed, err)
	}
	hash := sha256.Sum256([]byte(key))
	var stored []byte
	if err := pool.QueryRow(t.Context(), `SELECT hash FROM api_keys WHERE id = $1`, keys[0].ID).Scan(&stored); err != nil || !bytes.Equal(hash[:], stored) {
		t.Fatal("database did not store the SHA-256 key hash", err)
	}
	if len(key) != 47 || keys[0].Prefix != key[:12] || keys[0].Expires == nil || keys[0].Expires.Sub(keys[0].Created) < 7*24*time.Hour-time.Second ||
		keys[0].Expires.Sub(keys[0].Created) > 7*24*time.Hour+time.Second || strings.Contains(listed, key) || strings.Contains(listed, fmt.Sprintf("%x", hash)) {
		t.Fatal("key metadata or configured lifetime is incorrect")
	}
	var persisted string
	if err := pool.QueryRow(t.Context(), `SELECT row_to_json(k)::text FROM api_keys k WHERE id = $1`, keys[0].ID).Scan(&persisted); err != nil || strings.Contains(persisted, key) {
		t.Fatal("database contains plaintext key", err)
	}
	logs := &authLog{}
	base, stop := startRouter(t, binary, config, database.dsn, logs)
	otherBase, stopOther := startRouter(t, binary, config, database.dsn, logs)
	request := func(base, path, header, credential string, want int) {
		t.Helper()
		method := "POST"
		if path == "/v1/models" || strings.HasSuffix(path, "z") {
			method = "GET"
		}
		req, err := http.NewRequestWithContext(t.Context(), method, base+path,
			strings.NewReader(`{"model":"chat-model","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set(header, credential)
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s = %d %s, err=%v; want %d", path, resp.StatusCode, body, err, want)
		}
		if want == 401 {
			var shape struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if json.Unmarshal(body, &shape) != nil || shape.Error.Type != "authentication_error" ||
				(strings.Contains(path, "/messages") != (shape.Type == "error")) {
				t.Fatalf("wrong authentication error shape: %s", body)
			}
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/models"} {
		request(base, path, "", "", 401)
		request(base, path, "Authorization", "Bearer malformed", 401)
		unknown, _ := auth.Generate()
		request(base, path, "X-Api-Key", unknown, 401)
		request(base, path, "Authorization", "Bearer "+key, 200)
		request(otherBase, path, "X-Api-Key", key, 200)
	}
	request(base, "/healthz", "", "", 200)
	request(base, "/readyz", "", "", 200)
	var firstUse, secondUse time.Time
	if err := pool.QueryRow(t.Context(), `SELECT last_used FROM api_keys WHERE id = $1`, keys[0].ID).Scan(&firstUse); err != nil {
		t.Fatal(err)
	}
	// A cold cache in a third process still must not update last_used twice in
	// a minute. This checks the SQL guard, rather than only the local TTL.
	third, err := auth.New(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := third.Authenticate(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	third.Close()
	if err := pool.QueryRow(t.Context(), `SELECT last_used FROM api_keys WHERE id = $1`, keys[0].ID).Scan(&secondUse); err != nil || !firstUse.Equal(secondUse) {
		t.Fatal("last_used updated more than once per minute", err)
	}
	cli("key", "revoke", "-id", keys[0].ID)
	// No retry or polling: the very next request to each warm router must fail.
	request(base, "/v1/chat/completions", "Authorization", "Bearer "+key, 401)
	request(otherBase, "/v1/messages", "X-Api-Key", key, 401)
	secondKey := cli("key", "create", "-user", owner, "-lifetime", "1h")
	request(base, "/v1/models", "X-Api-Key", secondKey, 200)
	request(otherBase, "/v1/models", "X-Api-Key", secondKey, 200)
	cli("user", "disable", "-id", owner)
	request(base, "/v1/chat/completions", "Authorization", "Bearer "+secondKey, 401)
	request(otherBase, "/v1/messages", "X-Api-Key", secondKey, 401)
	// Expired credentials are checked against Postgres, independently of cache
	// expiry. Manually age a fresh key belonging to another, enabled user.
	expiredOwner := cli("user", "add", "-username", "expired")
	expiredKey := cli("key", "create", "-user", expiredOwner)
	execSQL(t, pool, `UPDATE api_keys SET expires = now() - interval '1 second' WHERE user_id = $1`, expiredOwner)
	request(base, "/v1/chat/completions", "Authorization", "Bearer "+expiredKey, 401)
	request(base, "/v1/messages", "X-Api-Key", expiredKey, 401)
	stop()
	stopOther()
	for _, secretKey := range []string{key, secondKey, expiredKey} {
		hash := sha256.Sum256([]byte(secretKey))
		for _, secret := range []string{secretKey, fmt.Sprintf("%x", hash), fmt.Sprintf("%v", hash[:])} {
			if strings.Contains(logs.String(), secret) {
				t.Fatal("router logs contain plaintext key or hash")
			}
		}
	}
}
