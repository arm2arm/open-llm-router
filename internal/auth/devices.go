package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const LocalDeviceCookie = "olr_device"
const LocalDeviceLifetime = 30 * 24 * time.Hour

// IssueLocalDeviceTx registers a random cookie after local password verification,
// or through the database-authorized operator recovery path. It grants only a
// separate rate budget. At most eight unexpired devices are retained per user.
func IssueLocalDeviceTx(ctx context.Context, tx pgx.Tx, userID string, previous string) (string, error) {
	var id string
	if err := tx.QueryRow(ctx, `SELECT u.id FROM users u JOIN local_credentials c ON c.user_id=u.id
		WHERE u.id=$1 AND u.issuer='local' AND u.enabled FOR UPDATE OF u`, userID).Scan(&id); err != nil {
		return "", err
	}
	if previous != "" {
		hash := sha256.Sum256([]byte(previous))
		result, err := tx.Exec(ctx, `UPDATE local_auth_devices SET expires=clock_timestamp()+interval '30 days'
			WHERE user_id=$1 AND hash=$2 AND expires>clock_timestamp()`, userID, hash[:])
		if err != nil {
			return "", err
		}
		if result.RowsAffected() == 1 {
			return previous, nil
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM local_auth_devices WHERE user_id=$1 AND expires<=clock_timestamp()`, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM local_auth_devices WHERE hash IN
		(SELECT hash FROM local_auth_devices WHERE user_id=$1 ORDER BY created DESC,hash OFFSET 7)`, userID); err != nil {
		return "", err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(raw))
	_, err := tx.Exec(ctx, `INSERT INTO local_auth_devices(hash,user_id,expires)
		VALUES($1,$2,clock_timestamp()+interval '30 days')`, hash[:], userID)
	return raw, err
}

// CreateLocalDevice is an audited operator operation. The caller must have
// database access; the HTTP login path issues devices inside its session tx.
func (a Admin) CreateLocalDevice(ctx context.Context, userID string) (raw string, err error) {
	a, finish := a.auditOperation(ctx, "local_admin.device", "user", userID)
	defer func() { finish(err) }()
	err = a.transaction(ctx, func(tx pgx.Tx) error {
		var issueErr error
		raw, issueErr = IssueLocalDeviceTx(ctx, tx, userID, "")
		return issueErr
	})
	if err != nil {
		return "", errors.New("local device registration failed")
	}
	return raw, nil
}

// KnownLocalDevice cannot authenticate a session or authorize any operation.
// Binding it to the submitted account prevents using another user's budget.
func (a Admin) KnownLocalDevice(ctx context.Context, username string, hash [32]byte) (bool, error) {
	var known bool
	err := a.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM local_auth_devices d JOIN users u ON u.id=d.user_id
		WHERE d.hash=$1 AND d.expires>clock_timestamp() AND u.issuer='local' AND u.sub=$2 AND u.enabled)`, hash[:], username).Scan(&known)
	if err != nil {
		return false, errors.New("local device lookup failed")
	}
	return known, nil
}
