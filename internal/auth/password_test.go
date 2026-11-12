package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordsAreSaltedAndBounded(t *testing.T) {
	password := []byte("a long private passphrase")
	first, err := hashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := hashPassword(password)
	if err != nil || first == second || strings.Contains(first, string(password)) {
		t.Fatal("password hashes must be salted and contain no plaintext")
	}
	if !verifyPassword(1, first, password) || verifyPassword(1, first, []byte("wrong password")) {
		t.Fatal("password verification")
	}
	for _, encoded := range []string{
		"", "invalid", first + "$extra", strings.Replace(first, "m=65536", "m=4294967295", 1),
		strings.Replace(first, "t=3", "t=0", 1), strings.Replace(first, "p=2", "p=255", 1),
		strings.Replace(first, "v=19", "v=16", 1), passwordPrefix + "!$!",
	} {
		if verifyPassword(1, encoded, password) {
			t.Fatal("malformed/unsupported credential accepted")
		}
	}
	if verifyPassword(2, first, password) || verifyPassword(1, first, []byte(strings.Repeat("x", maxPasswordBytes+1))) {
		t.Fatal("unsupported version or oversized password accepted")
	}
	for _, password := range [][]byte{nil, []byte("short"), []byte("密碼密碼密碼密碼"), []byte(strings.Repeat("x", maxPasswordBytes+1)), {0xff, 0xfe}} {
		if _, err := hashPassword(password); !errors.Is(err, ErrPasswordPolicy) {
			t.Fatal("password policy was not enforced")
		}
	}
	for _, password := range []string{strings.Repeat("x", 15), strings.Repeat("密", 15), strings.Repeat("x", maxPasswordBytes)} {
		if !validPassword([]byte(password)) {
			t.Fatal("valid password boundary rejected")
		}
	}
}

func BenchmarkPasswordHash(b *testing.B) {
	password := []byte("a long private benchmark passphrase")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := hashPassword(password); err != nil {
			b.Fatal(err)
		}
	}
}
