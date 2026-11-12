package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestAdminValidation(t *testing.T) {
	for _, args := range [][]string{
		{"bootstrap", "unexpected"},
		{"recover"},
		{"settings", "unknown"},
		{"settings", "show", "-domain", "routing"},
		{"settings", "show", "extra"},
		{"settings", "draft", "-document", "doc.json"},
		{"settings", "draft", "-expect-version", "1"},
		{"settings", "draft", "-expect-version", "1", "-document", "doc.json", "-secret", "portal_client_secret"},
		{"settings", "draft", "-expect-version", "1", "-document", "doc.json", "-secret", "a=x", "-secret", "a=y"},
		{"settings", "activate", "-expect-version", "1"},
		{"settings", "rollback", "-revision", "3"},
		{"settings", "discard"},
		{"settings", "show", "-config", ""},
		{"recover", "-password-stdin", "-password-file", "/private/new"},
		{"recover", "-password-stdin", "extra"},
		// Recovery has no account selector: it cannot target, disable,
		// delete or convert any identity, and takes no password flag.
		{"recover", "-password-stdin", "-user", "00000000-0000-0000-0000-000000000001"},
		{"recover", "-password-stdin", "-id", "00000000-0000-0000-0000-000000000001"},
		{"recover", "-password-stdin", "-disable"},
		{"recover", "-password", "a private password in argv"},
		{"password", "change"},
		{"password", "change", "-user", "00000000-0000-0000-0000-000000000001"},
		{"user", "add"},
		{"user", "add", "-username", "pilot", "-groups", "staff,,admins"},
		{"user", "disable", "-id", "pilot"},
		{"user", "delete"},
		{"user", "delete", "-id", "pilot"},
		{"user", "list", "extra"},
		{"key", "create", "-user", "00000000-0000-0000-0000-000000000001", "-class", "bulk"},
		{"override", "set", "-user", "00000000-0000-0000-0000-000000000001", "-field", "groups", "-value", "1"},
		{"override", "set", "-user", "00000000-0000-0000-0000-000000000001", "-field", "rpm", "-value", "0"},
		{"override", "set", "-user", "00000000-0000-0000-0000-000000000001", "-field", "strict_quota", "-value", "unlimited"},
		{"override", "set", "-user", "x", "-field", "rpm", "-value", "1"},
		{"override", "clear", "-user", "00000000-0000-0000-0000-000000000001"},
		{"override", "list"},
		{"key", "create", "-user", "bad-id"},
		{"key", "create", "-user", "00000000-0000-0000-0000-000000000001", "-lifetime", "0"},
		{"key", "create", "-user", "00000000-0000-0000-0000-000000000001", "-lifetime", "-1h"},
		{"key", "create", "-user", "00000000-0000-0000-0000-000000000001", "-lifetime", "2161h"},
		{"key", "list"},
		{"key", "revoke"},
		{"key", "revoke", "-id", "plaintext-is-not-an-id"},
		{"key", "list", "-user", "00000000-0000-0000-0000-000000000001", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("OLR_DATABASE_URL", "")
			t.Setenv("OLR_CONFIG", "")
			var out, errout bytes.Buffer
			err := admin(args, &out, &errout)
			if err == nil || strings.Contains(err.Error(), "OLR_DATABASE_URL") || out.Len() > 0 {
				t.Fatalf("validation must happen before connecting: err=%v stdout=%s", err, &out)
			}
		})
	}
}

func TestAdminHelp(t *testing.T) {
	var out, errout bytes.Buffer
	if err := admin([]string{"-h"}, &out, &errout); err != nil || !strings.Contains(out.String(), "key revoke") || errout.Len() != 0 {
		t.Fatalf("help: %v %s %s", err, &out, &errout)
	}
}
