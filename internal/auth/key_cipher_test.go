package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestKeyCipherAuthenticatedEncryption(t *testing.T) {
	cipher, err := LoadKeyCipher(func(string) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)) })
	if err != nil {
		t.Fatal(err)
	}
	key := "olr-private-synthetic-key"
	hash := sha256.Sum256([]byte(key))
	a, err := cipher.seal(key, "user", hash[:])
	if err != nil {
		t.Fatal(err)
	}
	b, _ := cipher.seal(key, "user", hash[:])
	if bytes.Equal(a, b) || bytes.Contains(a, []byte(key)) {
		t.Fatal("encryption not randomized")
	}
	plain, err := cipher.open(a, "user", hash[:])
	if err != nil || plain != key {
		t.Fatal("cannot decrypt")
	}
	if _, err := cipher.open(a, "different-user", hash[:]); err == nil {
		t.Fatal("owner swap accepted")
	}
	other := sha256.Sum256([]byte("other"))
	if _, err := cipher.open(a, "user", other[:]); err == nil {
		t.Fatal("hash swap accepted")
	}
	a[len(a)-1] ^= 1
	if _, err := cipher.open(a, "user", hash[:]); err == nil {
		t.Fatal("tampering accepted")
	}
	for _, raw := range []string{"secret-value", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := LoadKeyCipher(func(string) string { return raw }); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
