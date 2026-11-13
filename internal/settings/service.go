// Package settings keeps administrator-managed configuration in PostgreSQL:
// immutable revisions, one active revision and at most one draft per domain,
// encrypted secrets, and append-only audit events. A domain is file-managed
// until an explicit import makes the database its single authority.
//
// The managed domains are authentication (the SSO connection) and routing
// (logical models, their endpoints and aliases). Database/bootstrap
// credentials, encryption keys, listener and public-origin settings are
// deployment configuration and never managed here.
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

const (
	DomainAuthentication = "authentication"
	DomainRouting        = "routing"
)

var (
	ErrUnknownDomain = errors.New("unknown settings domain; the managed domains are authentication and routing")
	ErrNotManaged    = errors.New("these settings are file-managed; import them first")
	ErrManaged       = errors.New("these settings are already managed; change them with a draft instead of importing again")
	ErrConflict      = errors.New("settings changed since you loaded them; reload, then repeat your edit")
	ErrNoDraft       = errors.New("there is no draft")
	ErrRevision      = errors.New("activate the current draft, or roll back to a previously active revision")
	ErrStorage       = errors.New("managed settings storage is unavailable")
	ErrNoCipher      = errors.New("storing a secret requires " + EnvEncryptionKey)
)

// ValidationError lists field-level problems. Messages never contain secrets.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "settings are not valid:\n  " + strings.Join(e.Problems, "\n  ")
}

// Service changes and reports one domain's managed settings. File is this
// process's file/environment configuration, the reference for sources and
// conflicts.
type Service[D any, P document[D]] struct {
	DB     *pgxpool.Pool
	File   *config.Config
	Cipher *Cipher                     // nil: stored secrets cannot be saved or opened
	Lookup func(string) (string, bool) // environment for secret references; nil: os.LookupEnv
	// Current returns the configuration in use: the import baseline and the
	// base of candidate configurations. nil: File.
	Current func() *config.Config
	// Tokens resolves backend credential references; nil: Lookup.
	Tokens func(string) (string, bool)
	// Verify adds environment checks of a candidate configuration, such as
	// outbound destination policy; problems are recorded like validation.
	Verify func(context.Context, *config.Config) []string
}

// AuthenticationService manages the SSO connection; RoutingService manages
// logical models, their endpoints and aliases.
type (
	AuthenticationService  = Service[Authentication, *Authentication]
	RoutingService         = Service[Routing, *Routing]
	AuthenticationView     = View[Authentication]
	RoutingView            = View[Routing]
	AuthenticationRevision = Revision[Authentication]
)

type View[D any] struct {
	Domain    string        `json:"domain"`
	Managed   bool          `json:"managed"`
	Version   int64         `json:"version"` // pass as expected version to the next change
	Active    *Revision[D]  `json:"active,omitempty"`
	Draft     *Revision[D]  `json:"draft,omitempty"`
	Effective []Setting     `json:"effective"`
	Conflicts []Conflict    `json:"conflicts"`
	History   []Revision[D] `json:"history"`
}

// Summary returns the active and draft revision IDs and whether the draft's
// latest check passed.
func (v View[D]) Summary() (active, draft int64, draftValid bool) {
	if v.Active != nil {
		active = v.Active.ID
	}
	if v.Draft != nil {
		draft, draftValid = v.Draft.ID, v.Draft.Validation != nil && v.Draft.Validation.Valid
	}
	return active, draft, draftValid
}

type Revision[D any] struct {
	ID         int64         `json:"id"`
	Status     string        `json:"status"` // active, draft, previously_active or superseded
	Origin     string        `json:"origin"` // import or draft
	BasedOn    int64         `json:"based_on,omitempty"`
	CreatedBy  string        `json:"created_by"`
	CreatedAt  time.Time     `json:"created_at"`
	Document   *D            `json:"document,omitempty"` // secret references only
	Secrets    []SecretState `json:"secrets,omitempty"`
	Validation *Validation   `json:"validation,omitempty"`
	Checks     *Checks       `json:"checks,omitempty"`
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

// Conflict is a file or environment value that differs from the managed one.
// It names the setting and where it is set, never either value.
type Conflict struct {
	Setting string `json:"setting"`
	Source  string `json:"source"` // file or environment
	Where   string `json:"where"`  // e.g. oidc.issuer or OIDC_ISSUER
}

func (c Conflict) String() string {
	return fmt.Sprintf("%s: %s sets a different value; remove %s so managed settings are the only source", c.Setting, c.Where, c.Where)
}

type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type state struct{ active, draft, version int64 }

type revisionRow[D any] struct {
	Revision[D]
	doc *D
}

func (s *Service[D, P]) domain() string { return P(new(D)).domainName() }

func (s *Service[D, P]) checkDomain(domain string) error {
	if domain != s.domain() {
		return ErrUnknownDomain
	}
	return nil
}

func (s *Service[D, P]) lookup(name string) (string, bool) {
	if s.Lookup != nil {
		return s.Lookup(name)
	}
	return os.LookupEnv(name)
}

func (s *Service[D, P]) current() *config.Config {
	if s.Current != nil {
		if cfg := s.Current(); cfg != nil {
			return cfg
		}
	}
	return s.File
}

func (s *Service[D, P]) tokens() func(string) (string, bool) {
	if s.Tokens != nil {
		return s.Tokens
	}
	return s.lookup
}

func decodeDocument[D any, P document[D]](data []byte) (P, error) {
	doc := P(new(D))
	err := doc.decode(data)
	return doc, err
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

// Managed reports whether a domain is managed in the database.
func Managed(ctx context.Context, q querier, domain string) (bool, error) {
	_, managed, err := readState(ctx, q, domain, false)
	return managed, err
}

func (s *Service[D, P]) readRevision(ctx context.Context, q querier, id int64) (revisionRow[D], error) {
	var row revisionRow[D]
	var document, validation, checks []byte
	var basedOn *int64
	err := q.QueryRow(ctx, `SELECT id,origin,based_on,created_by_label,created_at,document,validation,checks
		FROM settings_revisions WHERE id=$1 AND domain=$2`, id, s.domain()).
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
	doc, err := decodeDocument[D, P](document)
	if err != nil {
		return row, errors.New("a stored settings revision is not readable by this version")
	}
	// Legacy routing revisions predate managed policies. Always resolve their
	// policies from the deployment baseline, never a newer active revision.
	if routing, ok := any(doc).(*Routing); ok && routing.Groups == nil {
		routing.Groups = s.File.Groups
	}
	row.doc = (*D)(doc)
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
func (s *Service[D, P]) resolve(ctx context.Context, q querier, doc P) (map[string]string, []SecretState, []string, error) {
	out := map[string]string{}
	var states []SecretState
	var problems []string
	for _, binding := range doc.secrets() {
		ref := *binding.ref
		st := SecretState{Setting: binding.name, Configured: !ref.empty()}
		var value string
		switch {
		case ref.Env != "":
			st.Reference, st.Env = "environment", ref.Env
			v, ok := s.lookup(ref.Env)
			if ok && v != "" && !strings.ContainsAny(v, "\r\n") {
				value, st.Available = v, true
			} else {
				problems = append(problems, fmt.Sprintf("%s: environment variable %s is not set to a single-line value", binding.name, ref.Env))
			}
		case ref.Stored != "":
			st.Reference, st.ID = "stored", ref.Stored
			var name, keyID string
			var sealed []byte
			var created time.Time
			err := q.QueryRow(ctx, `SELECT name,ciphertext,key_id,created_at FROM managed_secrets WHERE id=$1`, ref.Stored).Scan(&name, &sealed, &keyID, &created)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && name != s.domain()+"."+binding.name {
				problems = append(problems, binding.name+": the stored secret does not exist")
				break
			}
			if err != nil {
				return out, nil, nil, ErrStorage
			}
			st.CreatedAt = &created
			plain, err := s.Cipher.open(ref.Stored, name, keyID, sealed)
			if err != nil {
				problems = append(problems, binding.name+": "+errSecretUnavailable.Error())
				break
			}
			value, st.Available = string(plain), true
			clear(plain)
		}
		out[binding.name] = value
		states = append(states, st)
	}
	return out, states, problems, nil
}

// build resolves a document and applies it to base; problems never contain
// secrets.
func (s *Service[D, P]) build(ctx context.Context, q querier, doc P, base *config.Config) (*config.Config, map[string]string, []string, error) {
	problems := doc.problems()
	secrets, _, unresolved, err := s.resolve(ctx, q, doc)
	if err != nil {
		return nil, nil, nil, err
	}
	if problems = append(problems, unresolved...); len(problems) > 0 {
		return nil, secrets, problems, nil
	}
	cfg, err := doc.apply(base, applyInput{secrets: secrets, tokens: s.tokens()})
	if err != nil {
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			return nil, secrets, invalid.Problems, nil
		}
		return nil, secrets, []string{"the settings do not form a valid configuration"}, nil
	}
	if routing, ok := any(doc).(*Routing); ok && routing.Groups != nil {
		// Compare to the stored active policies too: CLI callers need not have
		// loaded the process's active routing snapshot. At startup this is the
		// same revision, so an old deployment baseline cannot block restoration.
		previous := s.current().Groups
		var activeGroups []byte
		err := q.QueryRow(ctx, `SELECT r.document->'groups' FROM settings_state s JOIN settings_revisions r ON r.id=s.active_revision WHERE s.domain='routing'`).Scan(&activeGroups)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil, ErrStorage
		}
		if err == nil && len(activeGroups) > 0 && string(activeGroups) != "null" {
			previous = nil
			if json.Unmarshal(activeGroups, &previous) != nil {
				return nil, nil, nil, ErrStorage
			}
		}
		for _, name := range slices.Sorted(maps.Keys(previous)) {
			if _, exists := routing.Groups[name]; exists {
				continue
			}
			var members int
			if err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE $1=ANY(groups) AND NOT protected_admin`, name).Scan(&members); err != nil {
				return nil, nil, nil, ErrStorage
			}
			if members > 0 {
				problems = append(problems, fmt.Sprintf("groups.%s: still referenced by %d users; reassign local memberships or update them at the identity provider before removing this group", name, members))
			}
		}
	}
	return cfg, secrets, problems, nil
}

// check validates a document against this process: secrets must resolve, the
// result must be a valid configuration that passes Verify, and (for domains
// whose conflicts block) no file or environment value may contradict it.
func (s *Service[D, P]) check(ctx context.Context, q querier, doc P) (Validation, error) {
	result := Validation{CheckedAt: time.Now().UTC()}
	cfg, secrets, problems, err := s.build(ctx, q, doc, s.current())
	if err != nil {
		return result, err
	}
	result.Problems = problems
	if cfg != nil && s.Verify != nil {
		result.Problems = append(result.Problems, s.Verify(ctx, cfg)...)
	}
	if doc.conflictsBlock() {
		for _, conflict := range doc.conflicts(s.File, secrets, len(problems) == 0) {
			result.Problems = append(result.Problems, conflict.String())
		}
	}
	result.Valid = len(result.Problems) == 0
	if result.Problems == nil {
		result.Problems = []string{}
	}
	return result, nil
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
func (s *Service[D, P]) change(ctx context.Context, domain string, event audit.Event, mutate func(pgx.Tx, *audit.Event) error) error {
	if err := s.checkDomain(domain); err != nil {
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

func (s *Service[D, P]) insertRevision(ctx context.Context, tx pgx.Tx, origin string, doc P, basedOn int64, actor audit.Actor, result Validation) (int64, error) {
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
		VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7) RETURNING id`, s.domain(), origin, document, parent, actor.ID, actor.Label(), validation).Scan(&id)
	if err != nil {
		return 0, ErrStorage
	}
	return id, nil
}

// Import makes the database the authority for a domain, starting from the
// current effective values without changing them.
func (s *Service[D, P]) Import(ctx context.Context, domain string, actor audit.Actor) (View[D], error) {
	err := s.change(ctx, domain, audit.Event{Action: "settings.import", Actor: actor}, func(tx pgx.Tx, event *audit.Event) error {
		if _, managed, err := readState(ctx, tx, domain, true); err != nil {
			return err
		} else if managed {
			return ErrManaged
		}
		doc := P(new(D))
		doc.baseline(s.current())
		result, err := s.check(ctx, tx, doc)
		if err != nil {
			return err
		}
		if !result.Valid {
			return &ValidationError{result.Problems}
		}
		id, err := s.insertRevision(ctx, tx, "import", doc, 0, actor, result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO settings_state(domain,active_revision,version) VALUES($1,$2,1)`, domain, id); err != nil {
			return ErrStorage
		}
		if err := recordActivation(ctx, tx, domain, id, "import", actor); err != nil {
			return err
		}
		counts := map[string]int{}
		for _, source := range doc.fileSources(s.File) {
			counts[source]++
		}
		event.Revision = id
		event.Details = map[string]any{"sources": counts}
		return nil
	})
	if err != nil {
		return View[D]{}, err
	}
	return s.View(ctx, domain)
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
// problems (unresolvable secrets, destinations, conflicts) are recorded on
// the draft and block only its activation.
func (s *Service[D, P]) SaveDraft(ctx context.Context, domain string, expected int64, document []byte, secrets map[string][]byte, actor audit.Actor) (View[D], error) {
	err := s.change(ctx, domain, audit.Event{Action: "settings.draft_save", Actor: actor}, func(tx pgx.Tx, event *audit.Event) error {
		doc, err := decodeDocument[D, P](document)
		if err != nil {
			return err
		}
		if routing, ok := any(doc).(*Routing); ok && routing.Groups == nil {
			routing.Groups = s.current().Groups
		}
		var problems []string
		bindings := doc.secrets()
		for _, name := range slices.Sorted(maps.Keys(secrets)) {
			if _, ok := bindingNamed(bindings, name); !ok {
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
		st, err := s.lockedState(ctx, tx, domain, expected)
		if err != nil {
			return err
		}
		active, err := s.readRevision(ctx, tx, st.active)
		if err != nil {
			return err
		}
		stored := []string{}
		for _, name := range slices.Sorted(maps.Keys(secrets)) {
			binding, _ := bindingNamed(bindings, name)
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
			*binding.ref = SecretRef{Stored: id}
			stored = append(stored, name)
		}
		result, err := s.check(ctx, tx, doc)
		if err != nil {
			return err
		}
		id, err := s.insertRevision(ctx, tx, "draft", doc, st.active, actor, result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE settings_state SET draft_revision=$2,version=version+1,updated_at=clock_timestamp() WHERE domain=$1`, domain, id); err != nil {
			return ErrStorage
		}
		event.Revision = id
		event.Details = map[string]any{"changed": changed[D, P](active.doc, doc), "secrets_stored": stored, "valid": result.Valid}
		return nil
	})
	if err != nil {
		return View[D]{}, err
	}
	return s.View(ctx, domain)
}

// Discard drops the pending draft; its revision remains in history.
func (s *Service[D, P]) Discard(ctx context.Context, domain string, expected int64, actor audit.Actor) (View[D], error) {
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
		return View[D]{}, err
	}
	return s.View(ctx, domain)
}

func (s *Service[D, P]) lockedState(ctx context.Context, tx pgx.Tx, domain string, expected int64) (state, error) {
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
// again. With requireChecks, the domain's stored checks must also allow it
// (for authentication: passed connection checks and a test sign-in when SSO
// is enabled). A refused activation records why and leaves the active
// revision. Routers apply the active revision when they start, or at once
// when activated from their Admin page.
func (s *Service[D, P]) Activate(ctx context.Context, domain string, expected, revision int64, actor audit.Actor, requireChecks bool) (View[D], error) {
	return s.activate(ctx, domain, expected, revision, actor, "activate", requireChecks)
}

// errChecksRequired explains why an unchecked draft cannot be activated.
const errChecksRequired = "run the connection checks (none may fail) and a successful test sign-in on this draft before activating it"

// Rollback re-activates a previously active revision after checking it again.
// A pending draft is kept.
func (s *Service[D, P]) Rollback(ctx context.Context, domain string, expected, revision int64, actor audit.Actor) (View[D], error) {
	return s.activate(ctx, domain, expected, revision, actor, "rollback", false)
}

func (s *Service[D, P]) activate(ctx context.Context, domain string, expected, revision int64, actor audit.Actor, kind string, requireChecks bool) (View[D], error) {
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
		row, err := s.readRevision(ctx, tx, revision)
		if err != nil {
			return err
		}
		active, err := s.readRevision(ctx, tx, st.active)
		if err != nil {
			return err
		}
		result, err := s.check(ctx, tx, row.doc)
		if err != nil {
			return err
		}
		if err := setValidation(ctx, tx, revision, result); err != nil {
			return err
		}
		event.Details = map[string]any{"previous_revision": st.active, "changed": changed[D, P](active.doc, row.doc)}
		if !result.Valid {
			return &ValidationError{result.Problems}
		}
		if requireChecks && !P(row.doc).checksPassed(row.Checks) {
			return &ValidationError{[]string{errChecksRequired}}
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
		return View[D]{}, err
	}
	return s.View(ctx, domain)
}

// View reports effective values with their sources, the active revision and
// draft with their checks, conflicts and recent history. It never returns a
// secret value.
func (s *Service[D, P]) View(ctx context.Context, domain string) (View[D], error) {
	if err := s.checkDomain(domain); err != nil {
		return View[D]{}, err
	}
	view := View[D]{Domain: domain, Effective: []Setting{}, Conflicts: []Conflict{}, History: []Revision[D]{}}
	st, managed, err := readState(ctx, s.DB, domain, false)
	if err != nil {
		return View[D]{}, err
	}
	if !managed {
		doc := P(new(D))
		doc.baseline(s.current())
		sources := doc.fileSources(s.File)
		view.Effective = effective[D, P](doc, s.secretStatesFromFile(doc), func(name string) string { return sources[name] })
		return view, nil
	}
	view.Managed, view.Version = true, st.version
	active, err := s.describe(ctx, st.active, "active")
	if err != nil {
		return View[D]{}, err
	}
	view.Active = &active.Revision
	secrets, states, problems, err := s.resolve(ctx, s.DB, active.doc)
	if err != nil {
		return View[D]{}, err
	}
	view.Effective = effective[D, P](active.doc, states, func(string) string { return "database" })
	if found := P(active.doc).conflicts(s.File, secrets, len(problems) == 0); found != nil {
		view.Conflicts = found
	}
	if st.draft != 0 {
		draft, err := s.describe(ctx, st.draft, "draft")
		if err != nil {
			return View[D]{}, err
		}
		view.Draft = &draft.Revision
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id,r.origin,COALESCE(r.based_on,0),r.created_by_label,r.created_at,
		EXISTS(SELECT 1 FROM settings_activations a WHERE a.revision_id=r.id)
		FROM settings_revisions r WHERE r.domain=$1 ORDER BY r.id DESC LIMIT 20`, domain)
	if err != nil {
		return View[D]{}, ErrStorage
	}
	defer rows.Close()
	for rows.Next() {
		var revision Revision[D]
		var activated bool
		if rows.Scan(&revision.ID, &revision.Origin, &revision.BasedOn, &revision.CreatedBy, &revision.CreatedAt, &activated) != nil {
			return View[D]{}, ErrStorage
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
		return View[D]{}, ErrStorage
	}
	return view, nil
}

func (s *Service[D, P]) describe(ctx context.Context, id int64, status string) (revisionRow[D], error) {
	row, err := s.readRevision(ctx, s.DB, id)
	if err != nil {
		return row, err
	}
	_, states, _, err := s.resolve(ctx, s.DB, row.doc)
	if err != nil {
		return row, err
	}
	row.Status, row.Document, row.Secrets = status, row.doc, states
	return row, nil
}

func (s *Service[D, P]) secretStatesFromFile(doc P) []SecretState {
	var states []SecretState
	for _, binding := range doc.secrets() {
		value, set := s.lookup(binding.env)
		st := SecretState{Setting: binding.name, Configured: set && value != ""}
		if st.Configured {
			st.Reference, st.Env, st.Available = "environment", binding.env, true
		}
		states = append(states, st)
	}
	return states
}

func effective[D any, P document[D]](doc P, secrets []SecretState, source func(string) string) []Setting {
	encoded, _ := json.Marshal(doc)
	var values map[string]any
	_ = json.Unmarshal(encoded, &values)
	bindings := doc.secrets()
	settings := []Setting{}
	for _, name := range doc.settingNames() {
		entry := Setting{Name: name, Source: source(name)}
		if _, secret := bindingNamed(bindings, name); secret {
			for i := range secrets {
				if secrets[i].Setting == name {
					entry.Secret = &secrets[i]
				}
			}
		} else {
			entry.Value = values[name]
		}
		settings = append(settings, entry)
	}
	return settings
}

// Snapshot is the immutable result of loading a domain at startup. Callers
// must not modify Config.
type Snapshot struct {
	Domain   string
	Revision int64  // 0: file-managed
	Problem  string // the active revision could not be applied; see fallback
	Config   *config.Config
}

// Apply returns the configuration to start with: base with this domain's
// active revision applied. A file-managed domain leaves base unchanged. A
// managed domain uses only its active revision; file values are never
// substituted. Conflicting file/environment values are ignored and logged.
// If the active revision cannot be applied, the domain's fallback decides:
// authentication turns SSO off (local login stays available), routing fails
// startup.
func (s *Service[D, P]) Apply(ctx context.Context, base *config.Config, log *slog.Logger) (Snapshot, error) {
	domain := s.domain()
	snapshot := Snapshot{Domain: domain, Config: base}
	st, managed, err := readState(ctx, s.DB, domain, false)
	if err != nil {
		return snapshot, errors.New("read managed settings failed")
	}
	if !managed {
		log.Info(domain + " settings are file-managed")
		return snapshot, nil
	}
	snapshot.Revision = st.active
	fail := func(problems []string) (Snapshot, error) {
		cfg, message, ok := P(new(D)).fallback(base)
		if !ok {
			return snapshot, fmt.Errorf("managed %s revision %d cannot be applied; roll back with olr admin settings rollback -domain %s:\n  %s",
				domain, st.active, domain, strings.Join(problems, "\n  "))
		}
		snapshot.Config, snapshot.Problem = cfg, strings.Join(problems, "; ")
		log.Error(message, "revision", st.active, "problems", problems)
		return snapshot, nil
	}
	row, err := s.readRevision(ctx, s.DB, st.active)
	if err != nil {
		return fail([]string{err.Error()})
	}
	cfg, secrets, problems, err := s.build(ctx, s.DB, row.doc, base)
	if err != nil {
		return snapshot, errors.New("read managed settings failed")
	}
	if len(problems) > 0 {
		return fail(problems)
	}
	snapshot.Config = cfg
	if found := P(row.doc).conflicts(s.File, secrets, true); len(found) > 0 {
		var described []string
		for _, conflict := range found {
			if P(row.doc).conflictsBlock() {
				described = append(described, conflict.String())
			} else {
				described = append(described, conflict.Setting)
			}
		}
		if P(row.doc).conflictsBlock() {
			log.Error("file or environment settings conflict with managed settings and are ignored", "domain", domain, "revision", st.active, "conflicts", described)
		} else {
			log.Info("the config file differs from the managed revision, which is used; the file is only the import baseline", "domain", domain, "revision", st.active, "differences", described)
		}
	}
	log.Info("managed "+domain+" settings applied", "revision", st.active)
	return snapshot, nil
}

// Apply loads the authentication domain onto file; see Service.Apply.
func Apply(ctx context.Context, db *pgxpool.Pool, file *config.Config, cipher *Cipher, lookup func(string) (string, bool), log *slog.Logger) (Snapshot, error) {
	s := &AuthenticationService{DB: db, File: file, Cipher: cipher, Lookup: lookup}
	return s.Apply(ctx, file, log)
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
// connection checks, test sign-in and activation. Revision 0 is the current
// effective configuration: the active revision when managed, otherwise the
// current configuration. Conflicts do not matter here; activation checks them.
func (s *Service[D, P]) Candidate(ctx context.Context, domain string, revision int64) (*config.Config, int64, error) {
	if err := s.checkDomain(domain); err != nil {
		return nil, 0, err
	}
	st, managed, err := readState(ctx, s.DB, domain, false)
	if err != nil {
		return nil, 0, err
	}
	if revision == 0 {
		if !managed {
			return s.current(), 0, nil
		}
		revision = st.active
	} else if !managed {
		return nil, 0, ErrNotManaged
	}
	row, err := s.readRevision(ctx, s.DB, revision)
	if err != nil {
		return nil, 0, err
	}
	cfg, _, problems, err := s.build(ctx, s.DB, row.doc, s.current())
	if err != nil {
		return nil, 0, err
	}
	if len(problems) > 0 {
		return nil, 0, &ValidationError{problems}
	}
	return cfg, revision, nil
}

// Document returns a revision's document (secret references only).
func (s *Service[D, P]) Document(ctx context.Context, revision int64) (*D, error) {
	row, err := s.readRevision(ctx, s.DB, revision)
	return row.doc, err
}

// RecordChecks stores connection check results (and/or a test sign-in) on a
// revision and audits them. Revision 0 (file-managed) is audited only.
func (s *Service[D, P]) RecordChecks(ctx context.Context, domain string, revision int64, connection []idpcheck.Result, signIn *idpcheck.Result, actor audit.Actor) error {
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

// Preview validates an unsaved routing/policy document without mutating state.
// It uses the same build and reference checks as draft activation.
func (s *Service[D, P]) Preview(ctx context.Context, document []byte) (*config.Config, error) {
	doc, err := decodeDocument[D, P](document)
	if err != nil {
		return nil, err
	}
	cfg, _, problems, err := s.build(ctx, s.DB, doc, s.current())
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, &ValidationError{problems}
	}
	return cfg, nil
}
