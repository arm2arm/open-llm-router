package integration_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgconn"
)

const managedFile = `
oidc:
  portal_client_id: olr-portal
  audience: olr-api
models:
  m:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: "http://backend.example:8000", weight: 1, max_inflight: 1}
`

func settingsKey(fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func managedEnvironment(key string) map[string]string {
	return map[string]string{"OIDC_ISSUER": "http://keycloak.example/realms/dev",
		config.EnvOIDCClientSecret: "file-portal-secret-value", settings.EnvEncryptionKey: key}
}

func managedConfig(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	lookup := func(name string) (string, bool) { value, ok := env[name]; return value, ok }
	cfg, err := config.Parse([]byte(managedFile), lookup)
	if err != nil {
		t.Fatal(err)
	}
	cfg.OIDCClientSecret, cfg.OIDCSyncSecret = env[config.EnvOIDCClientSecret], env[config.EnvOIDCSyncSecret]
	return cfg
}

func managedService(t *testing.T, env map[string]string, file *config.Config) *settings.Service {
	t.Helper()
	cipher, err := settings.LoadCipher(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	return &settings.Service{File: file, Cipher: cipher, Lookup: func(name string) (string, bool) { value, ok := env[name]; return value, ok }}
}

func TestManagedSettingsLifecycleSecretsAndHistory(t *testing.T) {
	database := newDatabase(t)
	pool := openDatabase(t, database.dsn)
	env := managedEnvironment(settingsKey(7))
	file := managedConfig(t, env)
	service := managedService(t, env, file)
	service.DB = pool
	ctx := t.Context()
	actor := audit.Actor{Type: "operator_cli", Name: "tester", Host: "ci"}
	secretValues := []string{"file-portal-secret-value", "stored-sync-secret-one", "stored-sync-secret-two"}
	apply := func(env map[string]string) settings.Snapshot {
		t.Helper()
		cipher, err := settings.LoadCipher(func(name string) string { return env[name] })
		if err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		snapshot, err := settings.Apply(ctx, pool, file, cipher, func(name string) (string, bool) { value, ok := env[name]; return value, ok },
			slog.New(slog.NewJSONHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secretValues {
			if strings.Contains(logs.String(), secret) {
				t.Fatal("startup log exposed a secret")
			}
		}
		return snapshot
	}

	// File-managed: effective values report their sources; edits need import.
	view, err := service.View(ctx, settings.DomainAuthentication)
	if err != nil || view.Managed || view.Version != 0 {
		t.Fatal("file-managed view", err, view)
	}
	sources := map[string]string{}
	for _, setting := range view.Effective {
		sources[setting.Name] = setting.Source
	}
	if sources["issuer"] != "environment" || sources["audience"] != "file" || sources["groups_claim"] != "default" || sources["portal_client_secret"] != "environment" {
		t.Fatal("sources", sources)
	}
	if _, err := service.SaveDraft(ctx, settings.DomainAuthentication, 1, []byte(`{}`), nil, actor); !errors.Is(err, settings.ErrNotManaged) {
		t.Fatal("draft before import", err)
	}
	if _, err := service.View(ctx, "routing"); !errors.Is(err, settings.ErrUnknownDomain) {
		t.Fatal("unknown domain", err)
	}

	// Import keeps behavior: the applied configuration equals the file one.
	before := apply(env)
	if before.Revision != 0 || before.Config != file {
		t.Fatal("file-managed startup must use the file configuration")
	}
	view, err = service.Import(ctx, settings.DomainAuthentication, actor)
	if err != nil || !view.Managed || view.Version != 1 || view.Active == nil || view.Active.Origin != "import" || len(view.Conflicts) != 0 {
		t.Fatal("import", err, view)
	}
	imported := view.Active.ID
	if view.Active.Document.PortalClientSecret != (settings.SecretRef{Env: config.EnvOIDCClientSecret}) {
		t.Fatal("imported secret is not a reference to its environment variable")
	}
	after := apply(env)
	if after.Revision != imported || after.Problem != "" || *after.Config.OIDC != *file.OIDC || after.Config.OIDCClientSecret != file.OIDCClientSecret {
		t.Fatal("import changed the effective configuration", after.Problem)
	}
	if _, err := service.Import(ctx, settings.DomainAuthentication, actor); !errors.Is(err, settings.ErrManaged) {
		t.Fatal("second import", err)
	}

	document := func(mutate func(*settings.Authentication)) []byte {
		doc := *view.Active.Document
		mutate(&doc)
		data, _ := json.Marshal(doc)
		return data
	}
	// A draft contradicting the environment is saved but cannot activate.
	view, err = service.SaveDraft(ctx, settings.DomainAuthentication, 1, document(func(a *settings.Authentication) {
		a.Issuer = "http://other.example/realms/test"
	}), nil, actor)
	if err != nil || view.Version != 2 || view.Draft == nil || view.Draft.Validation.Valid ||
		!strings.Contains(strings.Join(view.Draft.Validation.Problems, " "), "OIDC_ISSUER") || view.Active.ID != imported {
		t.Fatal("conflicting draft", err, view.Draft)
	}
	conflicting := view.Draft.ID
	_, err = service.Activate(ctx, settings.DomainAuthentication, 2, conflicting, actor, false)
	var invalid *settings.ValidationError
	if !errors.As(err, &invalid) || strings.Contains(err.Error(), "other.example") {
		t.Fatal("conflicting activation", err)
	}
	if snapshot := apply(env); snapshot.Revision != imported {
		t.Fatal("a refused activation changed the active revision")
	}
	// Documents with intrinsic problems, unknown secrets or another variable
	// as a secret reference are refused before anything is stored.
	for _, bad := range [][]byte{
		[]byte(`{"enabled":true,"issuer":"not a url","portal_client_id":"x"}`),
		document(func(a *settings.Authentication) { a.PortalClientSecret = settings.SecretRef{Env: "OLR_DATABASE_URL"} }),
		[]byte(`{"unknown":"field"}`),
	} {
		if _, err := service.SaveDraft(ctx, settings.DomainAuthentication, 2, bad, nil, actor); !errors.As(err, &invalid) {
			t.Fatal("invalid document saved", err)
		}
	}
	if _, err := service.SaveDraft(ctx, settings.DomainAuthentication, 2, document(func(*settings.Authentication) {}),
		map[string][]byte{"issuer": []byte("x")}, actor); !errors.As(err, &invalid) {
		t.Fatal("unknown secret setting accepted", err)
	}
	// Stale versions conflict instead of overwriting.
	if _, err := service.SaveDraft(ctx, settings.DomainAuthentication, 1, document(func(*settings.Authentication) {}), nil, actor); !errors.Is(err, settings.ErrConflict) {
		t.Fatal("stale draft", err)
	}

	// Store a sync secret and change an unmanaged-by-file value; activate.
	view, err = service.SaveDraft(ctx, settings.DomainAuthentication, 2, document(func(a *settings.Authentication) { a.SyncIntervalS = 300 }),
		map[string][]byte{"sync_client_secret": []byte(secretValues[1])}, actor)
	if err != nil || !view.Draft.Validation.Valid || view.Draft.Document.SyncClientSecret.Stored == "" {
		t.Fatal("valid draft", err, view.Draft)
	}
	first := view.Draft.ID
	if _, err := service.Activate(ctx, settings.DomainAuthentication, view.Version, conflicting, actor, false); !errors.Is(err, settings.ErrRevision) {
		t.Fatal("activated a revision that is not the draft", err)
	}
	view, err = service.Activate(ctx, settings.DomainAuthentication, view.Version, first, actor, false)
	if err != nil || view.Active.ID != first || view.Draft != nil {
		t.Fatal("activate", err)
	}
	snapshot := apply(env)
	if snapshot.Revision != first || snapshot.Config.OIDCSyncSecret != secretValues[1] || snapshot.Config.OIDC.SyncIntervalS != 300 {
		t.Fatal("activated revision not applied at startup")
	}
	// Rotate: a new value creates a new stored secret; the old one remains for rollback.
	view, err = service.SaveDraft(ctx, settings.DomainAuthentication, view.Version, document(func(*settings.Authentication) {}),
		map[string][]byte{"sync_client_secret": []byte(secretValues[2])}, actor)
	if err != nil {
		t.Fatal("rotation draft", err)
	}
	view, err = service.Activate(ctx, settings.DomainAuthentication, view.Version, view.Draft.ID, actor, false)
	if err != nil || apply(env).Config.OIDCSyncSecret != secretValues[2] {
		t.Fatal("rotation", err)
	}
	// Roll back to the first stored secret, then to the import baseline.
	if _, err := service.Rollback(ctx, settings.DomainAuthentication, view.Version, conflicting, actor); !errors.Is(err, settings.ErrRevision) {
		t.Fatal("rolled back to a never-active revision", err)
	}
	view, err = service.Rollback(ctx, settings.DomainAuthentication, view.Version, first, actor)
	if err != nil || apply(env).Config.OIDCSyncSecret != secretValues[1] {
		t.Fatal("rollback", err)
	}
	view, err = service.Rollback(ctx, settings.DomainAuthentication, view.Version, imported, actor)
	if err != nil || apply(env).Config.OIDCSyncSecret != "" || *apply(env).Config.OIDC != *file.OIDC {
		t.Fatal("rollback to import", err)
	}
	statuses := map[int64]string{}
	for _, revision := range view.History {
		statuses[revision.ID] = revision.Status
	}
	if statuses[imported] != "active" || statuses[first] != "previously_active" || statuses[conflicting] != "superseded" {
		t.Fatal("history statuses", statuses)
	}
	// Discarding a draft keeps its revision in history.
	view, err = service.SaveDraft(ctx, settings.DomainAuthentication, view.Version, document(func(*settings.Authentication) {}), nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	if view, err = service.Discard(ctx, settings.DomainAuthentication, view.Version, actor); err != nil || view.Draft != nil {
		t.Fatal("discard", err)
	}
	if _, err := service.Discard(ctx, settings.DomainAuthentication, view.Version, actor); !errors.Is(err, settings.ErrNoDraft) {
		t.Fatal("discard without draft", err)
	}

	// Plaintext never leaves the process: not in the view, revisions,
	// secret ciphertexts or audit events.
	encoded, _ := json.Marshal(view)
	var stored strings.Builder
	for _, query := range []string{`SELECT document::text||COALESCE(validation::text,'') FROM settings_revisions`,
		`SELECT encode(ciphertext,'escape')||name FROM managed_secrets`,
		`SELECT concat_ws(' ',action,actor_name,target_id,reason,details::text) FROM audit_events`} {
		rows, err := pool.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				t.Fatal(err)
			}
			stored.WriteString(text + "\n")
		}
		rows.Close()
	}
	for _, secret := range secretValues {
		if strings.Contains(string(encoded), secret) || strings.Contains(stored.String(), secret) {
			t.Fatal("a secret left the storage boundary")
		}
	}
	// Audit history records every change, with refusals, and is append-only.
	var actions []string
	rows, err := pool.Query(ctx, `SELECT action||':'||result FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action string
		_ = rows.Scan(&action)
		actions = append(actions, action)
	}
	rows.Close()
	joined := strings.Join(actions, " ")
	for _, want := range []string{"settings.import:succeeded", "settings.import:refused", "settings.draft_save:refused", "settings.activate:refused",
		"settings.activate:succeeded", "settings.rollback:succeeded", "settings.rollback:refused", "settings.draft_discard:succeeded"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing audit event", want, joined)
		}
	}
	for _, statement := range []string{`UPDATE audit_events SET reason='rewritten'`, `DELETE FROM audit_events`, `TRUNCATE audit_events`,
		`UPDATE settings_revisions SET document='{}'`, `DELETE FROM settings_revisions`, `UPDATE managed_secrets SET name='authentication.x'`,
		`DELETE FROM managed_secrets`, `DELETE FROM settings_activations`} {
		var refused *pgconn.PgError
		if _, err := pool.Exec(ctx, statement); !errors.As(err, &refused) || refused.ConstraintName != "append_only_history" {
			t.Fatal("history mutation allowed:", statement, err)
		}
	}

	// Restart with the wrong or no encryption key while a stored secret is
	// active: SSO is off, nothing falls back to file values, startup succeeds.
	view, err = service.Rollback(ctx, settings.DomainAuthentication, view.Version, first, actor)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{settingsKey(9), ""} {
		snapshot := apply(managedEnvironment(key))
		if snapshot.Revision != first || snapshot.Problem == "" || snapshot.Config.OIDC != nil || snapshot.Config.OIDCSyncSecret != "" || snapshot.Config.OIDCClientSecret != "" {
			t.Fatal("undecryptable secret did not disable SSO safely", snapshot.Problem)
		}
	}
	// A conflicting environment at startup is rejected; the active revision applies.
	conflict := managedEnvironment(settingsKey(7))
	conflict[config.EnvOIDCSyncSecret] = "environment-sync-secret"
	conflictFile := managedConfig(t, conflict)
	cipher, _ := settings.LoadCipher(func(name string) string { return conflict[name] })
	var logs bytes.Buffer
	snapshot, err = settings.Apply(ctx, pool, conflictFile, cipher, func(name string) (string, bool) { value, ok := conflict[name]; return value, ok },
		slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil || snapshot.Config.OIDCSyncSecret != secretValues[1] || !strings.Contains(logs.String(), "sync_client_secret: OIDC_SYNC_CLIENT_SECRET") ||
		strings.Contains(logs.String(), "environment-sync-secret") {
		t.Fatal("startup conflict handling", err, logs.String())
	}
}
