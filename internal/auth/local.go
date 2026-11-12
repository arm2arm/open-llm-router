package auth

import (
	"bytes"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const ProtectedAdminUsername = "admin"

var (
	ErrProtectedAdmin    = errors.New("protected local administrator cannot be deleted, disabled, demoted or detached from local credentials")
	ErrInvalidPassword   = errors.New("invalid local username or password")
	ErrBootstrapConflict = errors.New("local admin identity already exists without protection; bootstrap will not adopt an existing account")
	ErrPasswordReuse     = errors.New("new password must differ from the current password")
)

// LocalAccount contains only public identity metadata, never a password/hash.
// Verification is not permission to establish an unrestricted session while
// MustChangePassword is true. The local-login flow must require a password change.
type LocalAccount struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	ProtectedAdmin     bool   `json:"protected_admin"`
	MustChangePassword bool   `json:"must_change_password"`
	CredentialRevision int64  `json:"-"` // internal login snapshot, never a password/hash
}

func protectedAdminError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.ConstraintName == "protected_admin_guard" ||
		pgErr.ConstraintName == "protected_admin_identity" || pgErr.ConstraintName == "protected_admin_credentials_required")
}

func (a Admin) ProtectedLocalAdmin(ctx context.Context) (LocalAccount, error) {
	var account LocalAccount
	err := a.Pool.QueryRow(ctx, `SELECT u.id,u.username,u.protected_admin,c.must_change_password,c.revision
		FROM users u JOIN local_credentials c ON c.user_id=u.id WHERE u.protected_admin`).
		Scan(&account.ID, &account.Username, &account.ProtectedAdmin, &account.MustChangePassword, &account.CredentialRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalAccount{}, ErrNotFound
	}
	if err != nil {
		return LocalAccount{}, errors.New("protected administrator lookup failed")
	}
	return account, nil
}

// BootstrapLocalAdmin creates the reserved local administrator once. A database
// lock serializes concurrent callers; later calls ignore the supplied password.
// Existing unprotected identities are never adopted, including a local "admin".
func (a Admin) BootstrapLocalAdmin(ctx context.Context, password []byte) (LocalAccount, bool, error) {
	var account LocalAccount
	created := false
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		const bootstrapLock int64 = 0x4f4c5241444d494e // "OLRADMIN"
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", bootstrapLock); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT u.id,u.username,u.protected_admin,c.must_change_password,c.revision
			FROM users u JOIN local_credentials c ON c.user_id=u.id WHERE u.protected_admin`).
			Scan(&account.ID, &account.Username, &account.ProtectedAdmin, &account.MustChangePassword, &account.CredentialRevision)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var conflict bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE issuer='local' AND sub=$1)`, ProtectedAdminUsername).Scan(&conflict); err != nil {
			return err
		}
		if conflict {
			return ErrBootstrapConflict
		}
		hash, err := hashPassword(password)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO users(issuer,sub,username,protected_admin)
			VALUES('local',$1,$1,true) RETURNING id,username,protected_admin`, ProtectedAdminUsername).
			Scan(&account.ID, &account.Username, &account.ProtectedAdmin)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO local_credentials(user_id,version,password_hash,must_change_password)
			VALUES($1,$2,$3,true)`, account.ID, passwordVersion, hash); err != nil {
			return err
		}
		account.MustChangePassword, created = true, true
		account.CredentialRevision = 1
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrPasswordPolicy), errors.Is(err, ErrBootstrapConflict):
			return LocalAccount{}, false, err
		default:
			return LocalAccount{}, false, errors.New("protected administrator bootstrap failed")
		}
	}
	return account, created, nil
}

func (a Admin) VerifyLocalPassword(ctx context.Context, username string, password []byte) (LocalAccount, error) {
	var account LocalAccount
	var hash string
	var version int
	var enabled bool
	err := a.Pool.QueryRow(ctx, `SELECT u.id,u.username,u.protected_admin,c.must_change_password,c.revision,c.version,c.password_hash,u.enabled
		FROM users u JOIN local_credentials c ON c.user_id=u.id WHERE u.issuer='local' AND u.sub=$1`, username).
		Scan(&account.ID, &account.Username, &account.ProtectedAdmin, &account.MustChangePassword, &account.CredentialRevision, &version, &hash, &enabled)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !enabled) {
		_ = verifyPassword(passwordVersion, dummyPasswordHash, password)
		return LocalAccount{}, ErrInvalidPassword
	}
	if err != nil {
		return LocalAccount{}, errors.New("local password lookup failed")
	}
	if !verifyPassword(version, hash, password) {
		return LocalAccount{}, ErrInvalidPassword
	}
	return account, nil
}

// ChangeLocalPassword consumes the bootstrap password as well as supporting
// normal rotation. Compare-and-swap prevents concurrent changes from reviving an
// old credential. Existing sessions and API-key caches are invalidated together.
func (a Admin) ChangeLocalPassword(ctx context.Context, userID string, current, next []byte) error {
	if !validPassword(next) {
		return ErrPasswordPolicy
	}
	var hash string
	var version int
	err := a.Pool.QueryRow(ctx, `SELECT c.version,c.password_hash FROM local_credentials c
		JOIN users u ON u.id=c.user_id WHERE u.id=$1 AND u.issuer='local' AND u.enabled`, userID).Scan(&version, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidPassword
	}
	if err != nil {
		return errors.New("local password lookup failed")
	}
	if !verifyPassword(version, hash, current) {
		return ErrInvalidPassword
	}
	// The first change must actually replace the one-time bootstrap credential.
	if bytes.Equal(current, next) {
		return ErrPasswordReuse
	}
	replacement, err := hashPassword(next)
	if err != nil {
		return err
	}
	return a.changeWith(ctx, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE local_credentials c SET version=$2,password_hash=$3,
			must_change_password=false
			WHERE c.user_id=$1 AND c.password_hash=$4 AND EXISTS(SELECT 1 FROM users u WHERE u.id=c.user_id AND u.enabled AND u.issuer='local')`,
			userID, passwordVersion, replacement, hash)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrInvalidPassword
		}
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID)
		return err
	})
}
