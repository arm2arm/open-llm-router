package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresStore struct{ pool *pgxpool.Pool }

func (s postgresStore) lookup(ctx context.Context, hash [32]byte) (keyRecord, error) {
	var r keyRecord
	err := s.pool.QueryRow(ctx, `SELECT k.id, k.class, k.trusted_forwarder, u.id, u.username, u.groups, k.hash, k.expires, k.revoked, u.enabled,u.protected_admin,
		EXISTS(SELECT 1 FROM local_credentials c WHERE c.user_id=u.id AND c.must_change_password),
		(SELECT jsonb_object_agg(o.field, o.value) FROM quota_overrides o WHERE o.user_id = u.id)
		FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.hash = $1`, hash[:]).
		Scan(&r.KeyID, &r.KeyClass, &r.TrustedForwarder, &r.UserID, &r.Username, &r.Groups, &r.hash, &r.expires, &r.revoked, &r.enabled, &r.ProtectedAdmin, &r.passwordChangeRequired, &r.Overrides)
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
type Admin struct {
	Pool      *pgxpool.Pool
	KeyCipher *KeyCipher
	KeyPolicy config.Auth
}

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
	return a.SetEnabled(ctx, userID, false)
}

// SetEnabled disables or re-enables a user. Disabled users have no API or
// portal access; their sessions end at once.
func (a Admin) SetEnabled(ctx context.Context, userID string, enabled bool) error {
	return a.changeWith(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE users SET enabled=$2 WHERE id=$1`, userID, enabled)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return ErrNotFound
		}
		if !enabled {
			_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID)
		}
		return err
	})
}

// KeyInfo contains displayable metadata only. Hashes are deliberately excluded.
type KeyInfo struct {
	ID            string     `json:"id"`
	Prefix        string     `json:"prefix"`
	Label         string     `json:"label"`
	Created       time.Time  `json:"created"`
	LastUsed      *time.Time `json:"last_used"`
	Expires       *time.Time `json:"expires"`
	Revoked       bool       `json:"revoked"`
	Expired       bool       `json:"expired"`
	CopyAvailable bool       `json:"copy_available"`
	Class         string     `json:"class"`
	// TrustedForwarder keys may name the end user of a call.
	TrustedForwarder bool `json:"trusted_forwarder"`
}

var ErrKeyLifetime = errors.New("key lifetime exceeds site maximum")
var ErrKeyLimit = errors.New("active API key limit reached; revoke an existing key first")

// CreateKey returns a plaintext key. Admin copy can recover encrypted keys. A positive
// lifetime is mandatory; indefinite bootstrap keys cannot be created.
func (a Admin) CreateKey(ctx context.Context, userID, label string, lifetime time.Duration) (string, error) {
	return a.CreateKeyOfClass(ctx, userID, label, "interactive", lifetime)
}

// CreateForwarderKey creates a key for an application such as Open WebUI
// that may name the end user of each call (forwarders.header_map). Only
// operators create these; the portal cannot.
func (a Admin) CreateForwarderKey(ctx context.Context, userID, label, class string, lifetime time.Duration) (string, error) {
	return a.createKey(ctx, userID, label, class, lifetime, true)
}

// CreateKeyOfClass creates an "interactive" or a "batch" key; the classes
// have separate concurrency limits.
func (a Admin) CreateKeyOfClass(ctx context.Context, userID, label, class string, lifetime time.Duration) (string, error) {
	return a.createKey(ctx, userID, label, class, lifetime, false)
}

func (a Admin) createKey(ctx context.Context, userID, label, class string, lifetime time.Duration, trusted bool) (string, error) {
	maximum := a.KeyPolicy.MaxKeyLifetimeDays
	if maximum == 0 {
		maximum = config.DefaultMaxKeyLifetimeDays
	}
	cap := a.KeyPolicy.MaxActiveKeysPerUser
	if cap == 0 {
		cap = config.DefaultMaxActiveKeysPerUser
	}
	if maximum < 1 || maximum > 36500 || cap < 1 || cap > 1000 {
		return "", errors.New("invalid key issuance policy")
	}
	if lifetime > time.Duration(maximum)*24*time.Hour {
		return "", ErrKeyLifetime
	}
	if lifetime < time.Microsecond {
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
	err = pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 AND enabled FOR UPDATE`, userID).Scan(&id); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id=$1 AND NOT revoked AND (expires IS NULL OR expires>clock_timestamp())`, userID).Scan(&active); err != nil {
			return err
		}
		if active >= cap {
			return ErrKeyLimit
		}
		var sealed []byte
		var encryptionID *string
		if a.KeyCipher != nil {
			sealed, err = a.KeyCipher.seal(key, id, hash[:])
			if err != nil {
				return err
			}
			encryptionID = &a.KeyCipher.id
		}
		result, err := tx.Exec(ctx, `INSERT INTO api_keys (user_id, prefix, hash, label, expires, class, trusted_forwarder,key_ciphertext,key_encryption_id)
		SELECT id, $2, $3, $4, clock_timestamp() + $5::bigint * interval '1 microsecond', $6, $7,$8,$9
		FROM users WHERE id = $1 AND enabled AND NOT EXISTS
		(SELECT 1 FROM local_credentials c WHERE c.user_id=users.id AND c.must_change_password)`, userID, key[:12], hash[:], label, lifetime.Microseconds(), class, trusted, sealed, encryptionID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return errors.New("user not found, disabled, or password change required")
		}
		return nil
	})
	if errors.Is(err, ErrKeyLimit) {
		return "", ErrKeyLimit
	}
	if err != nil {
		return "", errors.New("user not found, disabled, password change required, or create API key failed")
	}
	return key, nil
}

func (a Admin) ListKeys(ctx context.Context, userID string) ([]KeyInfo, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, prefix, label, created, last_used, expires, revoked, class, trusted_forwarder,COALESCE(expires<=clock_timestamp(),false),
		COALESCE(key_encryption_id=$2 AND $2<>'' AND key_ciphertext IS NOT NULL AND NOT revoked AND (expires IS NULL OR expires>clock_timestamp()),false)
		FROM api_keys WHERE user_id = $1 ORDER BY created, id`, userID, a.cipherID())
	if err != nil {
		return nil, errors.New("list API keys failed")
	}
	defer rows.Close()
	keys := []KeyInfo{}
	for rows.Next() {
		var k KeyInfo
		if err := rows.Scan(&k.ID, &k.Prefix, &k.Label, &k.Created, &k.LastUsed, &k.Expires, &k.Revoked, &k.Class, &k.TrustedForwarder, &k.Expired, &k.CopyAvailable); err != nil {
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
	ID             string    `json:"id"`
	Issuer         string    `json:"issuer"`
	Subject        string    `json:"sub"`
	Username       string    `json:"username"`
	Email          string    `json:"email"`
	Groups         []string  `json:"groups"`
	Enabled        bool      `json:"enabled"`
	SyncedAt       time.Time `json:"synced_at"`
	ProtectedAdmin bool      `json:"protected_admin"`
}

// ListUsers returns users whose username or email contains match, ignoring
// case; an empty match lists everyone.
func (a Admin) ListUsers(ctx context.Context, match string) ([]UserInfo, error) {
	rows, err := a.Pool.Query(ctx, `SELECT id, issuer, sub, username, email, groups, enabled, synced_at, protected_admin FROM users
		WHERE $1 = '' OR strpos(lower(username), lower($1)) > 0 OR strpos(lower(email), lower($1)) > 0
		ORDER BY username, issuer, sub`, match)
	if err != nil {
		return nil, errors.New("list users failed")
	}
	defer rows.Close()
	users := []UserInfo{}
	for rows.Next() {
		var u UserInfo
		if err := rows.Scan(&u.ID, &u.Issuer, &u.Subject, &u.Username, &u.Email, &u.Groups, &u.Enabled, &u.SyncedAt, &u.ProtectedAdmin); err != nil {
			return nil, errors.New("read user failed")
		}
		users = append(users, u)
	}
	if rows.Err() != nil {
		return nil, errors.New("list users failed")
	}
	return users, nil
}

// EnsureIdentity returns the user for (issuer, sub), creating it or
// refreshing its claims only when they changed, so that frequent callers such
// as API tokens do not clear every router's caches. Disabled users give
// ErrNotFound.
func (a Admin) EnsureIdentity(ctx context.Context, issuer, sub, username, email string, groups []string) (string, error) {
	var id, oldName, oldEmail string
	var oldGroups []string
	var enabled bool
	err := a.Pool.QueryRow(ctx, `SELECT id, username, email, groups, enabled FROM users WHERE issuer = $1 AND sub = $2`, issuer, sub).
		Scan(&id, &oldName, &oldEmail, &oldGroups, &enabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return "", errors.New("user lookup failed")
	case !enabled:
		return "", ErrNotFound
	case oldName == username && oldEmail == email && slices.Equal(oldGroups, groups):
		return id, nil
	}
	return a.SyncIdentity(ctx, issuer, sub, username, email, groups)
}

// TokenIdentity maps an API access token to its router user. Group claims are
// as old as the token: when the stored memberships were updated after the
// token was issued (by a portal login, group sync or a newer token), the token
// can only narrow them, so a removed grant is never restored by an old token
// and an added group needs a new token. A token newer than the stored state
// updates it like a login. A token without iat counts as old.
func (a Admin) TokenIdentity(ctx context.Context, issuer, sub, username, email string, groups []string, issued time.Time) (string, []string, error) {
	var id, oldName, oldEmail string
	var oldGroups []string
	var enabled bool
	var synced time.Time
	err := a.Pool.QueryRow(ctx, `SELECT id,username,email,groups,enabled,synced_at FROM users WHERE issuer=$1 AND sub=$2`, issuer, sub).
		Scan(&id, &oldName, &oldEmail, &oldGroups, &enabled, &synced)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		id, err := a.SyncIdentity(ctx, issuer, sub, username, email, groups)
		return id, groups, err
	case err != nil:
		return "", nil, errors.New("user lookup failed")
	case !enabled:
		return "", nil, ErrNotFound
	case issued.IsZero() || !issued.After(synced):
		return id, slices.DeleteFunc(slices.Clone(groups), func(g string) bool { return !slices.Contains(oldGroups, g) }), nil
	case oldName == username && oldEmail == email && slices.Equal(oldGroups, groups):
		return id, groups, nil
	}
	id, err = a.SyncIdentity(ctx, issuer, sub, username, email, groups)
	return id, groups, err
}

// SetGroups replaces a user's groups; API-key caches observe it before this
// returns.
func (a Admin) SetGroups(ctx context.Context, userID string, groups []string) error {
	if groups == nil {
		groups = []string{}
	}
	return a.change(ctx, `UPDATE users SET groups = $2, synced_at = clock_timestamp() WHERE id = $1`, userID, groups)
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
