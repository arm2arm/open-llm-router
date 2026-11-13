package auth

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Local assignments serialize with policy activation. Deployment-file group
// checks belong to the caller; once managed, the committed revision is authority.
func validateManagedGroups(ctx context.Context, tx pgx.Tx, groups []string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1330398547,hashtext('routing'))`); err != nil {
		return err
	}
	var data []byte
	err := tx.QueryRow(ctx, `SELECT r.document->'groups' FROM settings_state s JOIN settings_revisions r ON r.id=s.active_revision WHERE s.domain='routing'`).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Legacy managed routing still uses deployment policies.
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var configured map[string]json.RawMessage
	if json.Unmarshal(data, &configured) != nil {
		return errors.New("managed group policies are unavailable")
	}
	for _, name := range groups {
		if _, ok := configured[name]; !ok {
			return errors.New("group is absent from the active managed policy")
		}
	}
	return nil
}
