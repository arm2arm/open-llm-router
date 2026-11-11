package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresStore struct{ pool *pgxpool.Pool }

func (s postgresStore) lookup(ctx context.Context, hash [32]byte) (keyRecord, error) {
	var r keyRecord
	err := s.pool.QueryRow(ctx, `SELECT k.id, u.id, u.username, u.groups, k.hash, k.expires, k.revoked, u.enabled
		FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.hash = $1`, hash[:]).
		Scan(&r.KeyID, &r.UserID, &r.Username, &r.Groups, &r.hash, &r.expires, &r.revoked, &r.enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrInvalidKey
	}
	if err != nil {
		return r, errors.New("API key lookup failed")
	}
	return r, nil
}

func (s postgresStore) touch(ctx context.Context, keyID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used = clock_timestamp()
		WHERE id = $1 AND (last_used IS NULL OR last_used <= clock_timestamp() - interval '1 minute')`, keyID)
	if err != nil {
		return errors.New("API key last-used update failed")
	}
	return nil
}

// Admin uses the same store as the server; it never returns database errors
// containing query arguments, credentials, or hashes to a caller or log.
type Admin struct{ Pool *pgxpool.Pool }

func (a Admin) AddUser(ctx context.Context, username, email string, groups []string) (string, error) {
	if strings.TrimSpace(username) == "" {
		return "", errors.New("username must be nonempty")
	}
	if groups == nil {
		groups = []string{}
	}
	var id string
	err := a.Pool.QueryRow(ctx, `INSERT INTO users (issuer, sub, username, email, groups)
		VALUES ('local', $1, $1, $2, $3) RETURNING id`, username, email, groups).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", errors.New("local user already exists")
	}
	if err != nil {
		return "", errors.New("create local user failed")
	}
	return id, nil
}

func (a Admin) DisableUser(ctx context.Context, userID string) error {
	return a.change(ctx, `UPDATE users SET enabled = false WHERE id = $1`, userID)
}

// KeyInfo contains displayable metadata only. Hashes are deliberately excluded.
type KeyInfo struct {
	ID       string     `json:"id"`
	Prefix   string     `json:"prefix"`
	Label    string     `json:"label"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used"`
	Expires  *time.Time `json:"expires"`
	Revoked  bool       `json:"revoked"`
}

// CreateKey is the only operation that returns a plaintext key. A positive
// lifetime is mandatory; indefinite bootstrap keys cannot be created.
func (a Admin) CreateKey(ctx context.Context, userID, label string, lifetime time.Duration) (string, error) {
	if lifetime <= 0 {
		return "", errors.New("key lifetime must be positive")
	}
	key, err := Generate()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(key))
	result, err := a.Pool.Exec(ctx, `INSERT INTO api_keys (user_id, prefix, hash, label, expires)
		SELECT id, $2, $3, $4, clock_timestamp() + $5::bigint * interval '1 microsecond'
		FROM users WHERE id = $1 AND enabled`, userID, key[:12], hash[:], label, lifetime.Microseconds())
	if err != nil {
		return "", errors.New("create API key failed")
	}
	if result.RowsAffected() == 0 {
		return "", errors.New("user not found or disabled")
	}
	return key, nil
}

func (a Admin) ListKeys(ctx context.Context, userID string) ([]KeyInfo, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, prefix, label, created, last_used, expires, revoked
		FROM api_keys WHERE user_id = $1 ORDER BY created, id`, userID)
	if err != nil {
		return nil, errors.New("list API keys failed")
	}
	defer rows.Close()
	keys := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.ID, &k.Prefix, &k.Label, &k.Created, &k.LastUsed, &k.Expires, &k.Revoked); err != nil {
			return nil, errors.New("read API key metadata failed")
		}
		keys = append(keys, k)
	}
	if rows.Err() != nil {
		return nil, errors.New("list API keys failed")
	}
	return keys, nil
}

func (a Admin) RevokeKey(ctx context.Context, keyID string) error {
	return a.change(ctx, `UPDATE api_keys SET revoked = true WHERE id = $1`, keyID)
}
