package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// EnvEncryptionKey supplies the key for stored secrets. It never lives in the
// database, so a database backup alone cannot reveal managed secrets.
const EnvEncryptionKey = "OLR_SETTINGS_ENCRYPTION_KEY"

var errSecretUnavailable = errors.New("stored secret cannot be decrypted with the configured " + EnvEncryptionKey)

// Cipher seals managed secret values with AES-256-GCM. The additional data
// binds a ciphertext to its secret id and name, so rows cannot be swapped.
type Cipher struct {
	aead cipher.AEAD
	id   string
}

// LoadCipher returns nil without a configured key: stored secrets can then be
// neither saved nor opened, while environment references keep working.
func LoadCipher(getenv func(string) string) (*Cipher, error) {
	raw := strings.TrimSpace(getenv(EnvEncryptionKey))
	if raw == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New(EnvEncryptionKey + " must be a base64-encoded 32-byte key")
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("settings encryption initialization failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("settings encryption initialization failed")
	}
	// The key id identifies which key sealed a row without revealing it.
	digest := sha256.Sum256(append([]byte("olr managed settings key id\x00"), key...))
	return &Cipher{aead: aead, id: hex.EncodeToString(digest[:12])}, nil
}

// ID is the public identifier of the key, stored next to each ciphertext.
func (c *Cipher) ID() string {
	if c == nil {
		return ""
	}
	return c.id
}

func secretAAD(id, name string) []byte {
	return []byte("olr managed secret v1\x00" + id + "\x00" + name)
}

func (c *Cipher) seal(id, name string, plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("secret encryption failed")
	}
	return c.aead.Seal(nonce, nonce, plain, secretAAD(id, name)), nil
}

func (c *Cipher) open(id, name, keyID string, sealed []byte) ([]byte, error) {
	if c == nil || keyID != c.id || len(sealed) < c.aead.NonceSize() {
		return nil, errSecretUnavailable
	}
	size := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, sealed[:size], sealed[size:], secretAAD(id, name))
	if err != nil {
		return nil, errSecretUnavailable
	}
	return plain, nil
}
