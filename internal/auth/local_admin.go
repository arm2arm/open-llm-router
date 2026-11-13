package auth

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrLocalUserExists = errors.New("local username already exists")
	ErrLocalIdentity   = errors.New("this operation requires an ordinary local identity")
	ErrLocalUserInput  = errors.New("use a username of 1 to 128 bytes without whitespace or control characters and a valid optional email address")
)

// CreateLocalUser never adopts an existing identity, including an API-key-only
// local account or an SSO account with the same username/email.
func (a Admin) CreateLocalUser(ctx context.Context, username, email string, groups []string, password []byte) (id string, err error) {
	a, finish := a.auditOperation(ctx, "user.create_local", "user", "")
	defer func() { finish(err) }()
	if username == ProtectedAdminUsername {
		return "", ErrProtectedAdmin
	}
	if username == "" || len(username) > 128 || !utf8.ValidString(username) || strings.ContainsFunc(username, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", ErrLocalUserInput
	}
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email || len(email) > 254 {
			return "", ErrLocalUserInput
		}
	}
	hash, err := hashPassword(password)
	if err != nil {
		return "", err
	}
	if groups == nil {
		groups = []string{}
	}
	err = a.transaction(ctx, func(tx pgx.Tx) error {
		if err := validateManagedGroups(ctx, tx, groups); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO users(issuer,sub,username,email,groups) VALUES('local',$1,$1,$2,$3) RETURNING id`, username, email, groups).Scan(&id); err != nil {
			return err
		}
		a.event.TargetID = id
		_, err := tx.Exec(ctx, `INSERT INTO local_credentials(user_id,version,password_hash,must_change_password) VALUES($1,$2,$3,true)`, id, passwordVersion, hash)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", ErrLocalUserExists
	}
	if err != nil {
		return "", errors.New("create password-enabled local user failed")
	}
	return id, nil
}

// Lock the identity before checking its source/protection. Session/key issuance
// takes a shared lock, so revocation cannot miss a concurrent old issuance.
func ordinaryLocal(ctx context.Context, tx pgx.Tx, id string) error {
	var issuer string
	var protected bool
	err := tx.QueryRow(ctx, `SELECT issuer,protected_admin FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&issuer, &protected)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return errors.New("local identity lookup failed")
	}
	if protected {
		return ErrProtectedAdmin
	}
	if issuer != "local" {
		return ErrLocalIdentity
	}
	return nil
}

func (a Admin) SetLocalGroups(ctx context.Context, id string, groups []string) (err error) {
	a, finish := a.auditOperation(ctx, "user.local_groups", "user", id)
	defer func() { finish(err) }()
	if groups == nil {
		groups = []string{}
	}
	return a.changeWith(ctx, func(tx pgx.Tx) error {
		if err := validateManagedGroups(ctx, tx, groups); err != nil {
			return err
		}
		if err := ordinaryLocal(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET groups=$2,synced_at=clock_timestamp() WHERE id=$1`, id, groups); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
		return err
	})
}

// ResetLocalPassword can also give an API-key-only local user a credential. The
// replacement is temporary: all sessions and keys are revoked, and the next
// local login must replace it. Protected-root recovery belongs to the operator.
func (a Admin) ResetLocalPassword(ctx context.Context, id string, password []byte) (err error) {
	a, finish := a.auditOperation(ctx, "credential.reset", "user", id)
	defer func() { finish(err) }()
	if !validPassword(password) {
		return ErrPasswordPolicy
	}
	return a.changeWith(ctx, func(tx pgx.Tx) error {
		if err := ordinaryLocal(ctx, tx, id); err != nil {
			return err
		}
		var version int
		var old string
		err := tx.QueryRow(ctx, `SELECT version,password_hash FROM local_credentials WHERE user_id=$1`, id).Scan(&version, &old)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && verifyPassword(version, old, password) {
			return ErrPasswordReuse
		}
		hash, err := hashPassword(password)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO local_credentials(user_id,version,password_hash,must_change_password) VALUES($1,$2,$3,true)
			ON CONFLICT(user_id) DO UPDATE SET version=EXCLUDED.version,password_hash=EXCLUDED.password_hash,must_change_password=true`, id, passwordVersion, hash); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT revision FROM local_credentials WHERE user_id=$1`, id).Scan(&a.event.Revision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked=true WHERE user_id=$1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
		return err
	})
}

// Operator revocation retains key metadata and is scoped to the displayed owner.
func (a Admin) RevokeOwnedKey(ctx context.Context, userID, keyID string) (err error) {
	a, finish := a.auditOperation(ctx, "key.revoke", "key", keyID)
	defer func() { finish(err) }()
	return a.change(ctx, `UPDATE api_keys SET revoked=true WHERE id=$1 AND user_id=$2`, keyID, userID)
}
