package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrKeyUnavailable = errors.New("this key cannot be copied; issue a replacement key with encryption configured")

// KeyCipher is supplied by the operator, separately from database backups.
// Authentication continues to use SHA-256; ciphertext is only for Admin copy.
type KeyCipher struct {
	aead cipher.AEAD
	id   string
}

func LoadKeyCipher(getenv func(string) string) (*KeyCipher, error) {
	raw := strings.TrimSpace(getenv("OLR_API_KEY_ENCRYPTION_KEY"))
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("OLR_API_KEY_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("API key encryption initialization failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("API key encryption initialization failed")
	}
	id := sha256.Sum256(key)
	return &KeyCipher{aead: aead, id: hex.EncodeToString(id[:12])}, nil
}
func (c *KeyCipher) seal(key, user string, hash []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("API key encryption failed")
	}
	return c.aead.Seal(nonce, nonce, []byte(key), append([]byte(user+":"), hash...)), nil
}
func (c *KeyCipher) open(sealed []byte, user string, hash []byte) (string, error) {
	if len(sealed) < c.aead.NonceSize() {
		return "", ErrKeyUnavailable
	}
	plain, err := c.aead.Open(nil, sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():], append([]byte(user+":"), hash...))
	if err != nil {
		return "", ErrKeyUnavailable
	}
	defer clear(plain)
	digest := sha256.Sum256(plain)
	if string(digest[:]) != string(hash) {
		return "", ErrKeyUnavailable
	}
	return string(plain), nil
}
func (a Admin) cipherID() string {
	if a.KeyCipher == nil {
		return ""
	}
	return a.KeyCipher.id
}

// CopyKey is only exposed through the session-authenticated Admin POST route.
// No secret is returned in list/detail operations or for inactive keys/users.
func (a Admin) CopyKey(ctx context.Context, userID, keyID string) (string, error) {
	if a.KeyCipher == nil {
		return "", ErrKeyUnavailable
	}
	var sealed, hash []byte
	var user, encryptionID string
	err := a.Pool.QueryRow(ctx, `SELECT k.key_ciphertext,k.hash,u.id,COALESCE(k.key_encryption_id,'')
		FROM api_keys k JOIN users u ON u.id=k.user_id
		WHERE k.id=$1 AND k.user_id=$2 AND NOT k.revoked
		AND (k.expires IS NULL OR k.expires>clock_timestamp()) AND u.enabled
		AND NOT EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=u.id AND c.must_change_password)`, keyID, userID).Scan(&sealed, &hash, &user, &encryptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", errors.New("copy API key failed")
	}
	if encryptionID != a.KeyCipher.id {
		return "", ErrKeyUnavailable
	}
	return a.KeyCipher.open(sealed, user, hash)
}
