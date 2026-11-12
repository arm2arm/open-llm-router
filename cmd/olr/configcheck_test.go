package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte("models:\n  m:\n    protocols: [openai]\n    timeout_s: 5\n    pool:\n      - {url: \"http://backend:8000\", weight: 1, max_inflight: 1}\naliases:\n  default: m\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("models:\n  m:\n    protocols: [grpc]\n    timeout_s: 0\n    pool: []\nunknown_key: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// No database URL is needed, and none is used.
	t.Setenv("OLR_DATABASE_URL", "postgres://unused.invalid/olr")
	var out, errout bytes.Buffer
	if err := configCommand([]string{"check", "-config", good}, &out, &errout); err != nil || !strings.Contains(out.String(), `"valid":true`) || !strings.Contains(out.String(), `"aliases":1`) {
		t.Fatal("valid file", err, out.String())
	}
	out.Reset()
	err := configCommand([]string{"check", "-config", bad}, &out, &errout)
	if err == nil || out.Len() != 0 || !strings.Contains(err.Error(), "unknown_key") {
		t.Fatal("invalid file", err)
	}
	if err := configCommand([]string{"check"}, &out, &errout); err == nil {
		t.Fatal("missing -config accepted")
	}
}
