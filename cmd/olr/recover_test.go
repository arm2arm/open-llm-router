package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/audit"
)

func TestRecoverPasswordStdin(t *testing.T) {
	password, err := readPasswordStdin(strings.NewReader("  piped recovery password  \r\n"))
	if err != nil || string(password) != "  piped recovery password  " {
		t.Fatal("piped password or line ending", err)
	}
	if _, err := readPasswordStdin(strings.NewReader(strings.Repeat("s", 1027))); err == nil {
		t.Fatal("oversized stdin password accepted")
	}
	// Device input (a terminal would echo) is refused before reading.
	device, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	if _, err := readPasswordStdin(device); err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Fatal("device stdin accepted", err)
	}
	// A redirected regular file is accepted.
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("redirected recovery password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if password, err := readPasswordStdin(file); err != nil || string(password) != "redirected recovery password" {
		t.Fatal("redirected file", err)
	}
}

// Attempts that fail before the database is reached are still audited, without
// the password or the file path.
func TestRecoverAuditsFailuresWithoutSecrets(t *testing.T) {
	t.Setenv("OLR_DATABASE_URL", "")
	t.Setenv(bootstrapPasswordEnv, "bootstrap value must be ignored")
	dir := t.TempDir()
	public := filepath.Join(dir, "public-password")
	if err := os.WriteFile(public, []byte("world readable recovery password"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		stdin  string
		result string
	}{
		{"public file", []string{"-password-file", public}, "", audit.Failed},
		{"no database", []string{"-password-stdin"}, "piped recovery password", audit.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errout bytes.Buffer
			err := recoverCommand(tc.args, strings.NewReader(tc.stdin), &out, &errout)
			if err == nil || out.Len() != 0 {
				t.Fatal("recovery succeeded without a database", err)
			}
			var line struct {
				Msg   string      `json:"msg"`
				Event audit.Event `json:"event"`
			}
			if err := json.Unmarshal(errout.Bytes(), &line); err != nil || line.Msg != "admin audit" ||
				line.Event.Action != recoverAction || line.Event.Result != tc.result || line.Event.Actor.Type != "operator_cli" || line.Event.Reason == "" {
				t.Fatalf("audit event: %v %s", err, &errout)
			}
			for _, secret := range []string{"world readable recovery password", "piped recovery password", public, "bootstrap value"} {
				if strings.Contains(errout.String(), secret) || strings.Contains(err.Error(), secret) {
					t.Fatal("recovery output exposed", secret)
				}
			}
		})
	}
}
