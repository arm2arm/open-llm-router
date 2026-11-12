// Package settings keeps administrator-managed configuration in PostgreSQL:
// immutable revisions, one active revision and at most one draft per domain,
// encrypted secrets, and append-only audit events. A domain is file-managed
// until an explicit import makes the database its single authority.
//
// Only the authentication domain (the SSO connection) is managed so far.
// Database/bootstrap credentials, encryption keys, listener and public-origin
// settings are deployment configuration and never managed here.
package settings

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/idpcheck"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DomainAuthentication = "authentication"

var (
	ErrUnknownDomain = errors.New("unknown settings domain; the managed domain is authentication")
	ErrNotManaged    = errors.New("these settings are file-managed; import them first")
	ErrManaged       = errors.New("these settings are already managed; change them with a draft instead of importing again")
	ErrConflict      = errors.New("settings changed since you loaded them; reload, then repeat your edit")
	ErrNoDraft       = errors.New("there is no draft")
	ErrRevision      = errors.New("activate the current draft, or roll back to a previously active revision")
	ErrStorage       = errors.New("managed settings storage is unavailable")
	ErrNoCipher      = errors.New("storing a secret requires " + EnvEncryptionKey)
)

// ValidationError lists field-level problems. Messages never contain values.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "settings are not valid:\n  " + strings.Join(e.Problems, "\n  ")
}

// Service changes and reports managed settings. File is this process's
// file/environment configuration: the import source, the base for candidate
// configurations and the reference for conflicts.
type Service struct {
	DB     *pgxpool.Pool
	File   *config.Config
	Cipher *Cipher                     // nil: stored secrets cannot be saved or opened
	Lookup func(string) (string, bool) // environment for secret references; nil: os.LookupEnv
}

type View struct {
	Domain    string     `json:"domain"`
	Managed   bool       `json:"managed"`
	Version   int64      `json:"version"` // pass as expected version to the next change
	Active    *Revision  `json:"active,omitempty"`
	Draft     *Revision  `json:"draft,omitempty"`
	Effective []Setting  `json:"effective"`
	Conflicts []Conflict `json:"conflicts"`
	History   []Revision `json:"history"`
}

type Revision struct {
	ID         int64           `json:"id"`
	Status     string          `json:"status"` // active, draft, previously_active or superseded
	Origin     string          `json:"origin"` // import or draft
	BasedOn    int64           `json:"based_on,omitempty"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	Document   *Authentication `json:"document,omitempty"` // secret references only
	Secrets    []SecretState   `json:"secrets,omitempty"`
	Validation *Validation     `json:"validation,omitempty"`
	Checks     *Checks         `json:"checks,omitempty"`
}

type Validation struct {
	Valid     bool      `json:"valid"`
	Problems  []string  `json:"problems"`
	CheckedAt time.Time `json:"checked_at"`
}

// Setting is one effective value and its source: database, file,
// environment or default. Secrets report their state, never their value.
type Setting struct {
	Name   string       `json:"name"`
	Value  any          `json:"value,omitempty"`
	Secret *SecretState `json:"secret,omitempty"`
	Source string       `json:"source"`
}

type SecretState struct {
	Setting    string     `json:"setting"`
	Configured bool       `json:"configured"`
	Reference  string     `json:"reference,omitempty"` // environment or stored
	Env        string     `json:"env,omitempty"`
	ID         string     `json:"id,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	Available  bool       `json:"available"` // the variable is set / the key opens it
}

type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type state struct{ active, draft, version int64 }

type revisionRow struct {
	Revision
	doc Authentication
}

func checkDomain(domain string) error {
	if domain != DomainAuthentication {
		return ErrUnknownDomain
	}
	return nil
}

func (s *Service) lookup(name string) (string, bool) {
	if s.Lookup != nil {
		return s.Lookup(name)
	}
	return os.LookupEnv(name)
}

func readState(ctx context.Context, q querier, domain string, lock bool) (state, bool, error) {
	if lock {
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(1330398547, hashtext($1))`, domain); err != nil {
			return state{}, false, ErrStorage
		}
	}
	var st state
	var draft *int64
	err := q.QueryRow(ctx, `SELECT active_revision,draft_revision,version FROM settings_state WHERE domain=$1`, domain).Scan(&st.active, &draft, &st.version)
	if errors.Is(err, pgx.ErrNoRows) {
		return state{}, false, nil
	}
	if err != nil {
		return state{}, false, ErrStorage
	}
	if draft != nil {
		st.draft = *draft
	}
	return st, true, nil
}

func readRevision(ctx context.Context, q querier, domain string, id int64) (revisionRow, error) {
	var row revisionRow
	var document, validation, checks []byte
	var basedOn *int64
	err := q.QueryRow(ctx, `SELECT id,origin,based_on,created_by_label,created_at,document,validation,checks
		FROM settings_revisions WHERE id=$1 AND domain=$2`, id, domain).
		Scan(&row.ID, &row.Origin, &basedOn, &row.CreatedBy, &row.CreatedAt, &document, &validation, &checks)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrRevision
	}
	if err != nil {
		return row, ErrStorage
	}
	if basedOn != nil {
		row.BasedOn = *basedOn
	}
	if row.doc, err = decodeAuthentication(document); err != nil {
		return row, errors.New("a stored settings revision is not readable by this version")
	}
	if len(validation) > 0 {
		row.Validation = &Validation{}
		if json.Unmarshal(validation, row.Validation) != nil {
			row.Validation = nil
		}
	}
	if len(checks) > 0 {
		row.Checks = &Checks{}
		if json.Unmarshal(checks, row.Checks) != nil {
			row.Checks = nil
		}
	}
	return row, nil
}

// resolve opens the document's secrets. Problems name the setting only.
func (s *Service) resolve(ctx context.Context, q querier, doc Authentication) (resolved, []SecretState, []string, error) {
	var out resolved
	var states []SecretState
	var problems []string
	for _, field := range authenticationSecrets {
		ref := *field.ref(&doc)
		st := SecretState{Setting: field.name, Configured: !ref.empty()}
		var value string
		switch {
		case ref.Env != "":
			st.Reference, st.Env = "environment", ref.Env
			v, ok := s.lookup(ref.Env)
			if ok && v != "" && !strings.ContainsAny(v, "\r\n") {
				value, st.Available = v, true
			} else {
				problems = append(problems, fmt.Sprintf("%s: environment variable %s is not set to a single-line value", field.name, ref.Env))
			}
		case ref.Stored != "":
			st.Reference, st.ID = "stored", ref.Stored
			var name, keyID string
			var sealed []byte
			var created time.Time
			err := q.QueryRow(ctx, `SELECT name,ciphertext,key_id,created_at FROM managed_secrets WHERE id=$1`, ref.Stored).Scan(&name, &sealed, &keyID, &created)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && name != DomainAuthentication+"."+field.name {
				problems = append(problems, field.name+": the stored secret does not exist")
				break
			}
			if err != nil {
				return out, nil, nil, ErrStorage
			}
			st.CreatedAt = &created
			plain, err := s.Cipher.open(ref.Stored, name, keyID, sealed)
			if err != nil {
				problems = append(problems, field.name+": "+errSecretUnavailable.Error())
				break
			}
			value, st.Available = string(plain), true
			clear(plain)
		}
		if field.name == "portal_client_secret" {
			out.portal = value
		} else {
			out.sync = value
		}
		states = append(states, st)
	}
	return out, states, problems, nil
}

// check validates a document against this process: secrets must resolve, the
// result must be a valid configuration, and no file or environment value may
// contradict it. Problems never contain values.
func (s *Service) check(ctx context.Context, q querier, doc Authentication) (Validation, []Conflict, error) {
	result := Validation{CheckedAt: time.Now().UTC(), Problems: doc.problems()}
	secrets, _, problems, err := s.resolve(ctx, q, doc)
	if err != nil {
		return result, nil, err
	}
	result.Problems = append(result.Problems, problems...)
	if len(result.Problems) == 0 {
		if _, err := doc.apply(s.File, secrets); err != nil {
			var invalid *ValidationError
			if errors.As(err, &invalid) {
				result.Problems = append(result.Problems, invalid.Problems...)
			}
		}
	}
	found := conflicts(s.File, doc, secrets, len(problems) == 0)
	for _, conflict := range found {
		result.Problems = append(result.Problems, conflict.String())
	}
	result.Valid = len(result.Problems) == 0
	if result.Problems == nil {
		result.Problems = []string{}
	}
	return result, found, nil
}

// changed lists the settings whose values or secret references differ.
func changed(before, after Authentication) []string {
	encode := func(doc Authentication) map[string]string {
		values := doc.values()
		for _, field := range authenticationSecrets {
			ref := *field.ref(&doc)
			values[field.name] = ref.Env + "|" + ref.Stored
		}
		return values
	}
	old, next := encode(before), encode(after)
	var names []string
	for _, setting := range config.AuthSettings {
		if old[setting.Name] != next[setting.Name] {
			names = append(names, setting.Name)
		}
	}
	if names == nil {
		names = []string{}
	}
	return names
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("generate secret id failed")
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func validSecret(value []byte) bool {
	return len(value) > 0 && len(value) <= 4096 && plainText(string(value), 4096)
}

// change runs a mutation under the domain lock. A refusal decided inside the
// transaction can still commit its validation record and audit event.
func (s *Service) change(ctx context.Context, domain string, event audit.Event, mutate func(pgx.Tx, *audit.Event) error) error {
	if err := checkDomain(domain); err != nil {
		return err
	}
	event.TargetType, event.TargetID = "settings", domain
	var refusal error
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if err := mutate(tx, &event); err != nil {
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				return err
			}
			refusal = err
			event.Result, event.Reason = audit.Refused, err.Error()
		} else {
			event.Result = audit.Succeeded
		}
		return audit.Record(ctx, tx, event)
	})
	if err != nil {
		known := []error{ErrNotManaged, ErrManaged, ErrConflict, ErrNoDraft, ErrRevision, ErrNoCipher}
		reason := ErrStorage
		for _, candidate := range known {
			if errors.Is(err, candidate) {
				reason = candidate
			}
		}
		event.Result, event.Reason, event.Details = audit.Refused, reason.Error(), nil
		if reason == ErrStorage {
			event.Result = audit.Failed
		}
		_ = audit.Record(ctx, s.DB, event) // best effort: the change itself rolled back
		return reason
	}
	return refusal
}

func setValidation(ctx context.Context, tx pgx.Tx, id int64, result Validation) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return ErrStorage
	}
	if _, err := tx.Exec(ctx, `UPDATE settings_revisions SET validation=$2 WHERE id=$1`, id, encoded); err != nil {
		return ErrStorage
	}
	return nil
}

func insertRevision(ctx context.Context, tx pgx.Tx, domain, origin string, doc Authentication, basedOn int64, actor audit.Actor, result Validation) (int64, error) {
	document, err := json.Marshal(doc)
	if err != nil {
		return 0, ErrStorage
	}
	validation, err := json.Marshal(result)
	if err != nil {
		return 0, ErrStorage
	}
	var parent *int64
	if basedOn > 0 {
		parent = &basedOn
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO settings_revisions(domain,origin,document,based_on,created_by,created_by_label,validation)
		VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7) RETURNING id`, domain, origin, document, parent, actor.ID, actor.Label(), validation).Scan(&id)
	if err != nil {
		return 0, ErrStorage
	}
	return id, nil
}

// Import makes the database the authority for a domain, starting from the
// current effective file and environment values without changing them.
func (s *Service) Import(ctx context.Context, domain string, actor audit.Actor) (View, error) {
	err := s.change(ctx, domain, audit.Event{Action: "settings.import", Actor: actor}, func(tx pgx.Tx, event *audit.Event) error {
		if _, managed, err := readState(ctx, tx, domain, true); err != nil {
			return err
		} else if managed {
			return ErrManaged
		}
		doc := authenticationFromFile(s.File)
		result, _, err := s.check(ctx, tx, doc)
		if err != nil {
			return err
		}
		if !result.Valid {
			return &ValidationError{result.Problems}
		}
		id, err := insertRevision(ctx, tx, domain, "import", doc, 0, actor, result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO settings_state(domain,active_revision,version) VALUES($1,$2,1)`, domain, id); err != nil {
			return ErrStorage
		}
		if err := recordActivation(ctx, tx, domain, id, "import", actor); err != nil {
			return err
		}
		event.Revision = id
		event.Details = map[string]any{"sources": sourceCounts(s.File)}
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.View(ctx, domain)
}

func sourceCounts(file *config.Config) map[string]int {
	counts := map[string]int{}
	for _, setting := range config.AuthSettings {
		counts[file.AuthSources[setting.Name]]++
	}
	return counts
}

func recordActivation(ctx context.Context, tx pgx.Tx, domain string, id int64, kind string, actor audit.Actor) error {
	_, err := tx.Exec(ctx, `INSERT INTO settings_activations(domain,revision_id,kind,activated_by,activated_by_label)
		VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5)`, domain, id, kind, actor.ID, actor.Label())
	if err != nil {
		return ErrStorage
	}
	return nil
}

// SaveDraft stores a new draft revision based on the active one. document is
// the domain's JSON document; secrets maps secret setting names to new
// plaintext values, which are encrypted and referenced by the draft (the
// rotation path). Document problems refuse the save; environment-dependent
// problems (unresolvable secrets, file/environment conflicts) are recorded
// on the draft and block only its activation.
func (s *Service) SaveDraft(ctx context.Context, domain string, expected int64, document []byte, secrets map[string][]byte, actor audit.Actor) (View, error) {
	err := s.change(ctx, domain, audit.Event{Action: "settings.draft_save", Actor: actor}, func(tx pgx.Tx, event *audit.Event) error {
		doc, err := decodeAuthentication(document)
		if err != nil {
			return err
		}
		var problems []string
		for _, name := range slices.Sorted(maps.Keys(secrets)) {
			if _, ok := secretFieldNamed(name); !ok {
				problems = append(problems, "secrets: one of the names is not a secret setting of this domain")
			} else if !validSecret(secrets[name]) {
				problems = append(problems, name+": a secret must be 1 to 4096 bytes of text without control characters")
			}
		}
		if problems = append(problems, doc.problems()...); len(problems) > 0 {
			return &ValidationError{problems}
		}
		if len(secrets) > 0 && s.Cipher == nil {
			return ErrNoCipher
		}
		st, managed, err := readState(ctx, tx, domain, true)
		if err != nil {
			return err
		}
		if !managed {
			return ErrNotManaged
		}
		if st.version != expected {
			return ErrConflict
		}
		active, err := readRevision(ctx, tx, domain, st.active)
		if err != nil {
			return err
		}
		stored := []string{}
		for _, name := range slices.Sorted(maps.Keys(secrets)) {
			field, _ := secretFieldNamed(name)
			id, err := newUUID()
			if err != nil {
				return ErrStorage
			}
			secretName := domain + "." + name
			sealed, err := s.Cipher.seal(id, secretName, secrets[name])
			if err != nil {
				return ErrStorage
			}
			if _, err := tx.Exec(ctx, `INSERT INTO managed_secrets(id,name,ciphertext,key_id,created_by) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid)`,
				id, secretName, sealed, s.Cipher.ID(), actor.ID); err != nil {
				return ErrStorage
			}
			*field.ref(&doc) = SecretRef{Stored: id}
			stored = append(stored, name)
		}
		result, _, err := s.check(ctx, tx, doc)
		if err != nil {
			return err
		}
		id, err := insertRevision(ctx, tx, domain, "draft", doc, st.active, actor, result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE settings_state SET draft_revision=$2,version=version+1,updated_at=clock_timestamp() WHERE domain=$1`, domain, id); err != nil {
			return ErrStorage
		}
		event.Revision = id
		event.Details = map[string]any{"changed": changed(active.doc, doc), "secrets_stored": stored, "valid": result.Valid}
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.View(ctx, domain)
}

// Discard drops the pending draft; its revision remains in history.
func (s *Service) Discard(ctx context.Context, domain string, expected int64, actor audit.Actor) (View, error) {
	err := s.change(ctx, domain, audit.Event{Action: "settings.draft_discard", Actor: actor}, func(tx pgx.Tx, event *audit.Event) error {
		st, err := s.lockedState(ctx, tx, domain, expected)
		if err != nil {
			return err
		}
		if st.draft == 0 {
			return ErrNoDraft
		}
		if _, err := tx.Exec(ctx, `UPDATE settings_state SET draft_revision=NULL,version=version+1,updated_at=clock_timestamp() WHERE domain=$1`, domain); err != nil {
			return ErrStorage
		}
		event.Revision = st.draft
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return s.View(ctx, domain)
}

func (s *Service) lockedState(ctx context.Context, tx pgx.Tx, domain string, expected int64) (state, error) {
	st, managed, err := readState(ctx, tx, domain, true)
	if err != nil {
		return st, err
	}
	if !managed {
		return st, ErrNotManaged
	}
	if st.version != expected {
		return st, ErrConflict
	}
	return st, nil
}

// Activate makes the current draft the active revision after checking it
// again. A refused activation records why and leaves the active revision.
// Routers apply the active revision when they start.
func (s *Service) Activate(ctx context.Context, domain string, expected, revision int64, actor audit.Actor) (View, error) {
	return s.activate(ctx, domain, expected, revision, actor, "activate")
}

// Rollback re-activates a previously active revision after checking it again.
// A pending draft is kept.
func (s *Service) Rollback(ctx context.Context, domain string, expected, revision int64, actor audit.Actor) (View, error) {
	return s.activate(ctx, domain, expected, revision, actor, "rollback")
}

func (s *Service) activate(ctx context.Context, domain string, expected, revision int64, actor audit.Actor, kind string) (View, error) {
	err := s.change(ctx, domain, audit.Event{Action: "settings." + kind, Actor: actor, Revision: revision}, func(tx pgx.Tx, event *audit.Event) error {
		st, err := s.lockedState(ctx, tx, domain, expected)
		if err != nil {
			return err
		}
		if kind == "activate" && (st.draft == 0 || revision != st.draft) {
			return ErrRevision
		}
		if kind == "rollback" {
			var eligible bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM settings_activations WHERE domain=$1 AND revision_id=$2)`, domain, revision).Scan(&eligible); err != nil {
				return ErrStorage
			}
			if !eligible || revision == st.active {
				return ErrRevision
			}
		}
		row, err := readRevision(ctx, tx, domain, revision)
		if err != nil {
			return err
		}
		active, err := readRevision(ctx, tx, domain, st.active)
		if err != nil {
			return err
		}
		result, _, err := s.check(ctx, tx, row.doc)
		if err != nil {
			return err
		}
		if err := setValidation(ctx, tx, revision, result); err != nil {
			return err
		}
		event.Details = map[string]any{"previous_revision": st.active, "changed": changed(active.doc, row.doc)}
		if !result.Valid {
			return &ValidationError{result.Problems}
		}
		draft := any(st.draft)
		if kind == "activate" || st.draft == 0 {
			draft = nil
		}
		if _, err := tx.Exec(ctx, `UPDATE settings_state SET active_revision=$2,draft_revision=$3,version=version+1,updated_at=clock_timestamp() WHERE domain=$1`,
			domain, revision, draft); err != nil {
			return ErrStorage
		}
		return recordActivation(ctx, tx, domain, revision, kind, actor)
	})
	if err != nil {
		return View{}, err
	}
	return s.View(ctx, domain)
}

// View reports effective values with their sources, the active revision and
// draft with their checks, conflicts and recent history. It never returns a
// secret value.
func (s *Service) View(ctx context.Context, domain string) (View, error) {
	if err := checkDomain(domain); err != nil {
		return View{}, err
	}
	view := View{Domain: domain, Effective: []Setting{}, Conflicts: []Conflict{}, History: []Revision{}}
	st, managed, err := readState(ctx, s.DB, domain, false)
	if err != nil {
		return View{}, err
	}
	if !managed {
		doc := authenticationFromFile(s.File)
		view.Effective = effective(doc, s.secretStatesFromFile(), func(name string) string { return s.File.AuthSources[name] })
		return view, nil
	}
	view.Managed, view.Version = true, st.version
	active, err := s.describe(ctx, domain, st.active, "active")
	if err != nil {
		return View{}, err
	}
	view.Active = &active.Revision
	secrets, states, problems, err := s.resolve(ctx, s.DB, active.doc)
	if err != nil {
		return View{}, err
	}
	view.Effective = effective(active.doc, states, func(string) string { return "database" })
	view.Conflicts = conflicts(s.File, active.doc, secrets, len(problems) == 0)
	if view.Conflicts == nil {
		view.Conflicts = []Conflict{}
	}
	if st.draft != 0 {
		draft, err := s.describe(ctx, domain, st.draft, "draft")
		if err != nil {
			return View{}, err
		}
		view.Draft = &draft.Revision
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id,r.origin,COALESCE(r.based_on,0),r.created_by_label,r.created_at,
		EXISTS(SELECT 1 FROM settings_activations a WHERE a.revision_id=r.id)
		FROM settings_revisions r WHERE r.domain=$1 ORDER BY r.id DESC LIMIT 20`, domain)
	if err != nil {
		return View{}, ErrStorage
	}
	defer rows.Close()
	for rows.Next() {
		var revision Revision
		var activated bool
		if rows.Scan(&revision.ID, &revision.Origin, &revision.BasedOn, &revision.CreatedBy, &revision.CreatedAt, &activated) != nil {
			return View{}, ErrStorage
		}
		switch {
		case revision.ID == st.active:
			revision.Status = "active"
		case revision.ID == st.draft:
			revision.Status = "draft"
		case activated:
			revision.Status = "previously_active"
		default:
			revision.Status = "superseded"
		}
		view.History = append(view.History, revision)
	}
	if rows.Err() != nil {
		return View{}, ErrStorage
	}
	return view, nil
}

func (s *Service) describe(ctx context.Context, domain string, id int64, status string) (revisionRow, error) {
	row, err := readRevision(ctx, s.DB, domain, id)
	if err != nil {
		return row, err
	}
	_, states, _, err := s.resolve(ctx, s.DB, row.doc)
	if err != nil {
		return row, err
	}
	doc := row.doc
	row.Status, row.Document, row.Secrets = status, &doc, states
	return row, nil
}

func (s *Service) secretStatesFromFile() []SecretState {
	var states []SecretState
	for _, field := range authenticationSecrets {
		value, set := s.lookup(field.env)
		st := SecretState{Setting: field.name, Configured: set && value != ""}
		if st.Configured {
			st.Reference, st.Env, st.Available = "environment", field.env, true
		}
		states = append(states, st)
	}
	return states
}

func effective(doc Authentication, secrets []SecretState, source func(string) string) []Setting {
	encoded, _ := json.Marshal(doc)
	var values map[string]any
	_ = json.Unmarshal(encoded, &values)
	settings := []Setting{}
	for _, setting := range config.AuthSettings {
		entry := Setting{Name: setting.Name, Source: source(setting.Name)}
		if _, secret := secretFieldNamed(setting.Name); secret {
			for i := range secrets {
				if secrets[i].Setting == setting.Name {
					entry.Secret = &secrets[i]
				}
			}
		} else {
			entry.Value = values[setting.Name]
		}
		settings = append(settings, entry)
	}
	return settings
}

// Snapshot is the immutable result of loading settings at startup. Callers
// must not modify Config.
type Snapshot struct {
	Domain   string
	Revision int64  // 0: file-managed
	Problem  string // the active revision could not be applied; SSO is off
	Config   *config.Config
}

// Apply returns the configuration a router starts with. A file-managed domain
// keeps the file configuration. A managed domain uses only its active
// revision: conflicting file/environment values are rejected and logged. If
// the active revision cannot be applied (for example, its stored secret
// cannot be decrypted), SSO is turned off and local login stays available;
// file values are never substituted.
func Apply(ctx context.Context, db *pgxpool.Pool, file *config.Config, cipher *Cipher, lookup func(string) (string, bool), log *slog.Logger) (Snapshot, error) {
	s := &Service{DB: db, File: file, Cipher: cipher, Lookup: lookup}
	snapshot := Snapshot{Domain: DomainAuthentication, Config: file}
	st, managed, err := readState(ctx, db, DomainAuthentication, false)
	if err != nil {
		return snapshot, errors.New("read managed settings failed")
	}
	if !managed {
		log.Info("authentication settings are file-managed")
		return snapshot, nil
	}
	snapshot.Revision = st.active
	disable := func(problems []string) (Snapshot, error) {
		cfg, err := config.WithAuthentication(file, nil, "", "")
		if err != nil {
			return snapshot, errors.New("local-only fallback configuration is invalid")
		}
		snapshot.Config, snapshot.Problem = cfg, strings.Join(problems, "; ")
		log.Error("managed authentication settings could not be applied; SSO is off and local login remains available",
			"revision", st.active, "problems", problems)
		return snapshot, nil
	}
	row, err := readRevision(ctx, db, DomainAuthentication, st.active)
	if err != nil {
		return disable([]string{err.Error()})
	}
	secrets, _, problems, err := s.resolve(ctx, db, row.doc)
	if err != nil {
		return snapshot, errors.New("read managed settings failed")
	}
	if problems = append(row.doc.problems(), problems...); len(problems) > 0 {
		return disable(problems)
	}
	cfg, err := row.doc.apply(file, secrets)
	if err != nil {
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			return disable(invalid.Problems)
		}
		return disable([]string{"the active revision does not form a valid configuration"})
	}
	snapshot.Config = cfg
	if found := conflicts(file, row.doc, secrets, true); len(found) > 0 {
		var described []string
		for _, conflict := range found {
			described = append(described, conflict.String())
		}
		log.Error("file or environment settings conflict with managed settings and are ignored", "revision", st.active, "conflicts", described)
	}
	log.Info("managed authentication settings applied", "revision", st.active, "sso", row.doc.Enabled)
	return snapshot, nil
}

// Checks are the latest connection checks and test sign-in of a revision.
type Checks struct {
	Connection *CheckRun        `json:"connection,omitempty"`
	TestSignIn *idpcheck.Result `json:"test_sign_in,omitempty"`
	TestedAt   *time.Time       `json:"test_sign_in_at,omitempty"`
}

type CheckRun struct {
	Results   []idpcheck.Result `json:"results"`
	CheckedAt time.Time         `json:"checked_at"`
}

// Passed reports whether every connection check passed or was skipped and
// the test sign-in passed.
func (c *Checks) Passed() bool {
	if c == nil || c.Connection == nil || c.TestSignIn == nil || c.TestSignIn.Status != idpcheck.Passed {
		return false
	}
	for _, result := range c.Connection.Results {
		if result.Status == idpcheck.Failed {
			return false
		}
	}
	return true
}

// Candidate returns the configuration a revision would run with, for
// connection checks and test sign-in. Revision 0 is the current effective
// configuration: the active revision when managed, otherwise the file.
// File/environment conflicts do not matter here; activation checks them.
func (s *Service) Candidate(ctx context.Context, domain string, revision int64) (*config.Config, int64, error) {
	if err := checkDomain(domain); err != nil {
		return nil, 0, err
	}
	st, managed, err := readState(ctx, s.DB, domain, false)
	if err != nil {
		return nil, 0, err
	}
	if revision == 0 {
		if !managed {
			return s.File, 0, nil
		}
		revision = st.active
	} else if !managed {
		return nil, 0, ErrNotManaged
	}
	row, err := readRevision(ctx, s.DB, domain, revision)
	if err != nil {
		return nil, 0, err
	}
	secrets, _, problems, err := s.resolve(ctx, s.DB, row.doc)
	if err != nil {
		return nil, 0, err
	}
	if problems = append(row.doc.problems(), problems...); len(problems) > 0 {
		return nil, 0, &ValidationError{problems}
	}
	cfg, err := row.doc.apply(s.File, secrets)
	return cfg, revision, err
}

// RecordChecks stores connection check results (and/or a test sign-in) on a
// revision and audits them. Revision 0 (file-managed) is audited only.
func (s *Service) RecordChecks(ctx context.Context, domain string, revision int64, connection []idpcheck.Result, signIn *idpcheck.Result, actor audit.Actor) error {
	action, details := "settings.check", map[string]any{}
	for _, result := range connection {
		details[result.Name] = result.Status
	}
	if signIn != nil {
		action, details = "settings.test_sign_in", map[string]any{signIn.Name: signIn.Status}
	}
	return s.change(ctx, domain, audit.Event{Action: action, Actor: actor, Revision: revision}, func(tx pgx.Tx, event *audit.Event) error {
		event.Details = details
		if revision == 0 {
			return nil
		}
		now := time.Now().UTC()
		patch := Checks{}
		if connection != nil {
			patch.Connection = &CheckRun{Results: connection, CheckedAt: now}
		}
		if signIn != nil {
			patch.TestSignIn, patch.TestedAt = signIn, &now
		}
		encoded, err := json.Marshal(patch)
		if err != nil {
			return ErrStorage
		}
		result, err := tx.Exec(ctx, `UPDATE settings_revisions SET checks=COALESCE(checks,'{}'::jsonb)||$3::jsonb WHERE id=$1 AND domain=$2`, revision, domain, encoded)
		if err != nil {
			return ErrStorage
		}
		if result.RowsAffected() != 1 {
			return ErrRevision
		}
		return nil
	})
}
