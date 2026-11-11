package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresStore struct{ pool *pgxpool.Pool }

func (s postgresStore) lookup(ctx context.Context, hash [32]byte) (keyRecord, error) {
	var r keyRecord
	err := s.pool.QueryRow(ctx, `SELECT k.id, k.class, u.id, u.username, u.groups, k.hash, k.expires, k.revoked, u.enabled,
		(SELECT jsonb_object_agg(o.field, o.value) FROM quota_overrides o WHERE o.user_id = u.id)
		FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.hash = $1`, hash[:]).
		Scan(&r.KeyID, &r.KeyClass, &r.UserID, &r.Username, &r.Groups, &r.hash, &r.expires, &r.revoked, &r.enabled, &r.Overrides)
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
	Class    string     `json:"class"`
}

// CreateKey is the only operation that returns a plaintext key. A positive
// lifetime is mandatory; indefinite bootstrap keys cannot be created.
func (a Admin) CreateKey(ctx context.Context, userID, label string, lifetime time.Duration) (string, error) {
	return a.CreateKeyOfClass(ctx, userID, label, "interactive", lifetime)
}

// CreateKeyOfClass creates an "interactive" or a "batch" key; the classes
// have separate concurrency limits.
func (a Admin) CreateKeyOfClass(ctx context.Context, userID, label, class string, lifetime time.Duration) (string, error) {
	if lifetime <= 0 {
		return "", errors.New("key lifetime must be positive")
	}
	if class != "interactive" && class != "batch" {
		return "", errors.New("key class must be interactive or batch")
	}
	key, err := Generate()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(key))
	result, err := a.Pool.Exec(ctx, `INSERT INTO api_keys (user_id, prefix, hash, label, expires, class)
		SELECT id, $2, $3, $4, clock_timestamp() + $5::bigint * interval '1 microsecond', $6
		FROM users WHERE id = $1 AND enabled`, userID, key[:12], hash[:], label, lifetime.Microseconds(), class)
	if err != nil {
		return "", errors.New("create API key failed")
	}
	if result.RowsAffected() == 0 {
		return "", errors.New("user not found or disabled")
	}
	return key, nil
}

func (a Admin) ListKeys(ctx context.Context, userID string) ([]KeyInfo, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, prefix, label, created, last_used, expires, revoked, class
		FROM api_keys WHERE user_id = $1 ORDER BY created, id`, userID)
	if err != nil {
		return nil, errors.New("list API keys failed")
	}
	defer rows.Close()
	keys := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.ID, &k.Prefix, &k.Label, &k.Created, &k.LastUsed, &k.Expires, &k.Revoked, &k.Class); err != nil {
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

// RemoveOwnedKey revokes an active key. A key that is already revoked or has
// expired is deleted for good; usage rows that referenced it keep their data
// without the key reference. It reports whether the key was deleted.
func (a Admin) RemoveOwnedKey(ctx context.Context, userID, keyID string) (deleted bool, err error) {
	err = a.changeWith(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE id=$1 AND user_id=$2
			AND (revoked OR expires <= clock_timestamp())`, keyID, userID)
		if err != nil {
			return errors.New("delete API key failed")
		}
		if result.RowsAffected() == 1 {
			deleted = true
			return nil
		}
		result, err = tx.Exec(ctx, `UPDATE api_keys SET revoked=true WHERE id=$1 AND user_id=$2`, keyID, userID)
		if err != nil {
			return errors.New("revoke API key failed")
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	return deleted, err
}

// DeleteUser erases a user with their keys, sessions, per-request records
// (including calls forwarded for them) and daily totals, in one transaction.
func (a Admin) DeleteUser(ctx context.Context, userID string) error {
	return a.change(ctx, `DELETE FROM users WHERE id = $1`, userID)
}

// UserInfo is what operators need to find a user; it holds no credentials.
type UserInfo struct {
	ID       string    `json:"id"`
	Issuer   string    `json:"issuer"`
	Subject  string    `json:"sub"`
	Username string    `json:"username"`
	Email    string    `json:"email"`
	Groups   []string  `json:"groups"`
	Enabled  bool      `json:"enabled"`
	SyncedAt time.Time `json:"synced_at"`
}

// ListUsers returns users whose username or email contains match, ignoring
// case; an empty match lists everyone.
func (a Admin) ListUsers(ctx context.Context, match string) ([]UserInfo, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, issuer, sub, username, email, groups, enabled, synced_at FROM users
		WHERE $1 = '' OR strpos(lower(username), lower($1)) > 0 OR strpos(lower(email), lower($1)) > 0
		ORDER BY username, issuer, sub`, match)
	if err != nil {
		return nil, errors.New("list users failed")
	}
	defer rows.Close()
	users := []UserInfo{}
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.ID, &u.Issuer, &u.Subject, &u.Username, &u.Email, &u.Groups, &u.Enabled, &u.SyncedAt); err != nil {
			return nil, errors.New("read user failed")
		}
		users = append(users, u)
	}
	if rows.Err() != nil {
		return nil, errors.New("list users failed")
	}
	return users, nil
}

// SyncIdentity refreshes claims without re-enabling disabled accounts or linking
// by email. API key caches observe new memberships before this returns.
func (a Admin) SyncIdentity(ctx context.Context, issuer, sub, username, email string, groups []string) (string, error) {
	if groups == nil {
		groups = []string{}
	}
	var id string
	err := a.changeWith(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO users(issuer,sub,username,email,groups,synced_at)
			VALUES($1,$2,$3,$4,$5,clock_timestamp()) ON CONFLICT(issuer,sub) DO UPDATE SET
			username=EXCLUDED.username,email=EXCLUDED.email,groups=EXCLUDED.groups,synced_at=EXCLUDED.synced_at
			WHERE users.enabled RETURNING id`, issuer, sub, username, email, groups).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return id, err
}

// Override is one per-user limit; a nil Value means unlimited.
type Override struct {
	Field  string    `json:"field"`
	Value  *int64    `json:"value"`
	Reason string    `json:"reason"`
	SetBy  string    `json:"set_by"`
	SetAt  time.Time `json:"set_at"`
}

// SetOverride replaces one field of a user's effective policy. Every router
// applies it to the next request, because the change clears the API-key caches.
func (a Admin) SetOverride(ctx context.Context, userID, field string, value *int64, reason, setBy string) error {
	if !policy.ValidOverride(field, value) {
		return errors.New("invalid override field or value")
	}
	return a.change(ctx, `INSERT INTO quota_overrides (user_id, field, value, reason, set_by, set_at)
		SELECT id, $2, $3, $4, $5, clock_timestamp() FROM users WHERE id = $1
		ON CONFLICT (user_id, field) DO UPDATE SET value = EXCLUDED.value, reason = EXCLUDED.reason,
		set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at`, userID, field, value, reason, setBy)
}

// ClearOverride returns one field to the group policy.
func (a Admin) ClearOverride(ctx context.Context, userID, field string) error {
	return a.change(ctx, `DELETE FROM quota_overrides WHERE user_id = $1 AND field = $2`, userID, field)
}

// ListOverrides returns a user's overrides by field.
func (a Admin) ListOverrides(ctx context.Context, userID string) ([]Override, error) {
	rows, err := a.Pool.Query(ctx, `SELECT field, value, reason, set_by, set_at FROM quota_overrides
		WHERE user_id = $1 ORDER BY field`, userID)
	if err != nil {
		return nil, errors.New("list overrides failed")
	}
	defer rows.Close()
	list := []Override{}
	for rows.Next() {
		var o Override
		if err := rows.Scan(&o.Field, &o.Value, &o.Reason, &o.SetBy, &o.SetAt); err != nil {
			return nil, errors.New("read override failed")
		}
		list = append(list, o)
	}
	if rows.Err() != nil {
		return nil, errors.New("list overrides failed")
	}
	return list, nil
}
