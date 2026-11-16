package settings

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// secretName binds a stored client secret to its complete connection. The
// backchannel can receive token requests instead of the public issuer host.
func (a *Authentication) secretName(setting string) string {
	client := a.PortalClientID
	if setting == "sync_client_secret" {
		client = a.SyncClientID
	}
	connection, _ := json.Marshal([]string{a.Issuer, a.BackchannelURL, client})
	hash := sha256.Sum256(connection)
	name := []byte(setting + "_")
	for _, b := range hash {
		name = append(name, 'a'+(b>>4), 'a'+(b&15))
	}
	return string(name)
}

func (s *Service[D, P]) storedSecretName(doc P, setting string) string {
	if authentication, ok := any(doc).(*Authentication); ok {
		return s.domain() + "." + authentication.secretName(setting)
	}
	return s.domain() + "." + setting
}

// originalConnection reads the first immutable revision that introduced a
// pre-binding stored secret. Later drafts cannot
// redefine that original recipient, including drafts saved before this fix.
func originalConnection(ctx context.Context, q querier, binding secretBinding, kind, ref string) (*Authentication, error) {
	var data []byte
	err := q.QueryRow(ctx, `SELECT document FROM settings_revisions
		WHERE domain='authentication' AND jsonb_extract_path_text(document,$1,$2)=$3
		ORDER BY id LIMIT 1`, binding.name, kind, ref).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrStorage
	}
	original := new(Authentication)
	if original.decode(data) != nil {
		return nil, ErrStorage
	}
	return original, nil
}

func (s *Service[D, P]) environmentConnectionAllowed(ctx context.Context, q querier, doc P, binding secretBinding) (bool, error) {
	authentication, ok := any(doc).(*Authentication)
	if !ok {
		return true, nil
	}
	// Only the import is deployment-authorized. An invalid draft must not
	// establish its own binding merely by being the first to name a variable.
	original := authenticationFromFile(s.File)
	var data []byte
	err := q.QueryRow(ctx, `SELECT document FROM settings_revisions WHERE domain='authentication' AND origin='import' ORDER BY id LIMIT 1`).Scan(&data)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, ErrStorage
	}
	if err == nil && original.decode(data) != nil {
		return false, ErrStorage
	}
	return original.secretName(binding.name) == authentication.secretName(binding.name), nil
}

func (s *Service[D, P]) storedConnectionAllowed(ctx context.Context, q querier, doc P, binding secretBinding, name string) (bool, error) {
	if name == s.storedSecretName(doc, binding.name) {
		return true, nil
	}
	authentication, ok := any(doc).(*Authentication)
	if !ok || name != s.domain()+"."+binding.name {
		return false, nil
	}
	original, err := originalConnection(ctx, q, binding, "stored", binding.ref.Stored)
	return err == nil && original != nil && original.secretName(binding.name) == authentication.secretName(binding.name), err
}
