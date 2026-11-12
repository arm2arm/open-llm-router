package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password format version 1 uses a fixed, bounded Argon2id work factor. Future
// versions can raise the cost without trusting arbitrary parameters from storage.
const passwordVersion = 1
const passwordPrefix = "$argon2id$v=19$m=65536,t=3,p=2$"
const maxPasswordBytes = 1024

var ErrPasswordPolicy = errors.New("password must contain at least 15 characters and at most 1024 bytes of valid UTF-8")

func validPassword(password []byte) bool {
	return len(password) <= maxPasswordBytes && utf8.Valid(password) && utf8.RuneCount(password) >= 15
}

func passwordKey(password, salt []byte) []byte {
	return argon2.IDKey(password, salt, 3, 64*1024, 2, 32)
}

func hashPassword(password []byte) (string, error) {
	if !validPassword(password) {
		return "", ErrPasswordPolicy
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", errors.New("password salt generation failed")
	}
	key := passwordKey(password, salt[:])
	defer clear(key)
	return passwordPrefix + base64.RawStdEncoding.EncodeToString(salt[:]) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func verifyPassword(version int, encoded string, password []byte) bool {
	if version != passwordVersion || len(password) > maxPasswordBytes || !strings.HasPrefix(encoded, passwordPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordPrefix), "$")
	if len(parts) != 2 || len(parts[0]) != 22 || len(parts[1]) != 43 {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return false
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(want) != 32 {
		return false
	}
	got := passwordKey(password, salt)
	defer clear(got)
	return subtle.ConstantTimeCompare(want, got) == 1
}

// Unknown/disabled accounts still perform the same bounded KDF. This is a dummy
// value, never a stored credential or an accepted password.
const dummyPasswordHash = passwordPrefix + "AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
