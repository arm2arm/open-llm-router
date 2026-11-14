// Package endpoints persists and activates Admin edits to existing model pools.
// Model names, aliases and group policies remain file-owned.
package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrConflict = errors.New("endpoint configuration changed; reload before applying your edit")
var ErrModel = errors.New("choose an existing configured model")
var ErrRevision = errors.New("revision does not belong to this model")
var ErrStorage = errors.New("endpoint configuration storage is unavailable")

// ErrRoutingManaged refuses per-model edits once routing is managed as
// complete revisions (Admin routing settings).
var ErrRoutingManaged = errors.New("routing is managed as revisions; change models and endpoints in a routing draft")

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

type Runtime interface {
	Snapshot() (*config.Config, *pool.Manager)
	Prepare(context.Context, *config.Config, string) (func(), []pool.State, error)
}

type Service struct {
	mu      sync.Mutex
	DB      *pgxpool.Pool
	Base    *config.Config
	Policy  *backendpolicy.Policy
	Runtime Runtime
}

type Revision struct {
	ID            int64         `json:"id"`
	Created       time.Time     `json:"created_at"`
	File          bool          `json:"file"`
	Configuration *config.Model `json:"configuration"`
}
type Entry struct {
	Model             string       `json:"model"`
	Configuration     config.Model `json:"configuration"`
	Revision          int64        `json:"revision"`
	Source            string       `json:"source"`
	History           []Revision   `json:"history"`
	FileConfiguration config.Model `json:"file_configuration"`
}

type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// VerifyDestinations checks every replica of a configuration against the
// outbound policy that proxy traffic also uses.
func VerifyDestinations(policy *backendpolicy.Policy) func(context.Context, *config.Config) []string {
	return func(ctx context.Context, cfg *config.Config) []string {
		var problems []string
		for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
			for i, r := range cfg.Models[name].Pool {
				if err := policy.CheckBackend(ctx, r.URL, r.PrivateNetwork); err != nil {
					problems = append(problems, fmt.Sprintf("models.%s.pool[%d]: %s", name, i, err))
				}
			}
		}
		return problems
	}
}

func loaded(ctx context.Context, db querier, base *config.Config, policy *backendpolicy.Policy) (*config.Config, map[string]int64, error) {
	// Managed routing already contains every model; per-model revisions no
	// longer apply on top of it.
	if managed, err := settings.Managed(ctx, db, settings.DomainRouting); err != nil {
		return nil, nil, ErrStorage
	} else if managed {
		return base, map[string]int64{}, nil
	}
	models := maps.Clone(base.Models)
	revisions := map[string]int64{}
	rows, err := db.Query(ctx, `SELECT a.model_name,a.revision_id,r.configuration FROM endpoint_active a JOIN endpoint_revisions r ON r.id=a.revision_id`)
	if err != nil {
		return nil, nil, ErrStorage
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var revision int64
		var body []byte
		if rows.Scan(&name, &revision, &body) != nil {
			return nil, nil, ErrStorage
		}
		if _, ok := models[name]; !ok {
			if len(body) == 0 {
				continue // a file-owned model may subsequently be removed from YAML
			}
			return nil, nil, errors.New("a managed endpoint model is absent from the configuration file")
		}
		if len(body) > 0 {
			var model config.Model
			if json.Unmarshal(body, &model) != nil {
				return nil, nil, ErrStorage
			}
			models[name] = model
		}
		revisions[name] = revision
	}
	if rows.Err() != nil {
		return nil, nil, ErrStorage
	}
	cfg, err := config.WithModels(base, models, policy.Token)
	return cfg, revisions, err
}

// Load applies persisted overrides at startup. File settings continue to own
// every field outside the explicitly managed model definitions.
func Load(ctx context.Context, db *pgxpool.Pool, base *config.Config, policy *backendpolicy.Policy) (*config.Config, error) {
	cfg, _, err := loaded(ctx, db, base, policy)
	return cfg, err
}

func (s *Service) View(ctx context.Context) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(ctx)
}
func (s *Service) view(ctx context.Context) ([]Entry, error) {
	cfg, revisions, err := loaded(ctx, s.DB, s.Base, s.Policy)
	if err != nil {
		return nil, err
	}
	entries := []Entry{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
		entry := Entry{Model: name, Configuration: cfg.Models[name], FileConfiguration: s.Base.Models[name], Revision: revisions[name], Source: "file", History: []Revision{}}
		rows, err := s.DB.Query(ctx, `SELECT id,created_at,configuration IS NULL,configuration FROM endpoint_revisions WHERE model_name=$1 ORDER BY id DESC LIMIT 20`, name)
		if err != nil {
			return nil, ErrStorage
		}
		for rows.Next() {
			var revision Revision
			var encoded []byte
			if rows.Scan(&revision.ID, &revision.Created, &revision.File, &encoded) != nil {
				rows.Close()
				return nil, ErrStorage
			}
			if len(encoded) > 0 {
				var model config.Model
				if json.Unmarshal(encoded, &model) != nil {
					rows.Close()
					return nil, ErrStorage
				}
				revision.Configuration = &model
			}
			entry.History = append(entry.History, revision)
			if revision.ID == entry.Revision && !revision.File {
				entry.Source = "database"
			}
		}
		rows.Close()
		if rows.Err() != nil {
			return nil, ErrStorage
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (s *Service) candidate(ctx context.Context, base *config.Config, name string, model config.Model) (*config.Config, error) {
	if _, ok := s.Base.Models[name]; !ok {
		return nil, ErrModel
	}
	if len(model.Pool) > 32 {
		return nil, &ValidationError{"Admin-managed models can have at most 32 replicas"}
	}
	models := maps.Clone(base.Models)
	models[name] = model
	cfg, err := config.WithModels(base, models, s.Policy.Token)
	if err != nil {
		return nil, err
	}
	for _, r := range cfg.Models[name].Pool {
		if err := s.Policy.CheckBackend(ctx, r.URL, r.PrivateNetwork); err != nil {
			return nil, &ValidationError{err.Error()}
		}
	}
	return cfg, nil
}

func (s *Service) Test(ctx context.Context, name string, model config.Model) ([]pool.State, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	base, _ := s.Runtime.Snapshot()
	cfg, err := s.candidate(ctx, base, name, model)
	if err != nil {
		return nil, err
	}
	_, states, err := s.Runtime.Prepare(ctx, cfg, name)
	if err != nil {
		err = &ValidationError{err.Error()}
	}
	return states, err
}

// Save serializes revisions in PostgreSQL and checks the caller's last observed
// revision. Health checks and validation finish before commit/publication.
// restore<0 saves model; restore==0 restores YAML; restore>0 restores history.
func (s *Service) Save(ctx context.Context, name string, expected int64, model config.Model, restore int64, actor string) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	actorInfo := audit.Actor{Type: "portal_user", ID: actor}
	if scoped, ok := audit.ActorFromContext(ctx); ok {
		actorInfo = scoped
	}
	event := audit.Event{Action: "endpoints.activate", Actor: actorInfo, TargetType: "model", TargetID: name, Result: audit.Succeeded}
	if restore >= 0 {
		event.Action = "endpoints.rollback"
	}
	var publish func()
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(571403029385094382)`); err != nil {
			return ErrStorage
		}
		base, revisions, err := loaded(ctx, tx, s.Base, s.Policy)
		if err != nil {
			return err
		}
		if managed, err := settings.Managed(ctx, tx, settings.DomainRouting); err != nil {
			return ErrStorage
		} else if managed {
			return ErrRoutingManaged
		}
		if _, ok := s.Base.Models[name]; !ok {
			return ErrModel
		}
		if expected != revisions[name] {
			return ErrConflict
		}
		var encoded []byte
		if restore >= 0 {
			if restore > 0 {
				if err := tx.QueryRow(ctx, `SELECT configuration FROM endpoint_revisions WHERE id=$1 AND model_name=$2`, restore, name).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
					return ErrRevision
				} else if err != nil {
					return ErrStorage
				}
			}
			model = s.Base.Models[name]
			if len(encoded) > 0 {
				var restored config.Model
				if json.Unmarshal(encoded, &restored) != nil {
					return ErrStorage
				}
				model = restored
			}
		}
		cfg, err := s.candidate(ctx, base, name, model)
		if err != nil {
			return err
		}
		publish, _, err = s.Runtime.Prepare(ctx, cfg, name)
		if err != nil {
			return &ValidationError{err.Error()}
		}
		if restore < 0 || len(encoded) > 0 {
			encoded, err = json.Marshal(cfg.Models[name])
			if err != nil {
				return ErrStorage
			}
		}
		var revision int64
		if err := tx.QueryRow(ctx, `INSERT INTO endpoint_revisions(model_name,configuration,created_by) VALUES($1,$2,$3) RETURNING id`, name, encoded, actor).Scan(&revision); err != nil {
			return ErrStorage
		}
		if _, err := tx.Exec(ctx, `INSERT INTO endpoint_active(model_name,revision_id) VALUES($1,$2) ON CONFLICT(model_name) DO UPDATE SET revision_id=EXCLUDED.revision_id`, name, revision); err != nil {
			return ErrStorage
		}
		event.Revision = revision
		return audit.Record(ctx, tx, event)
	})
	if err != nil {
		var validation *ValidationError
		var problems config.Problems
		event.Result, event.Reason = audit.Refused, "endpoint change refused"
		if !errors.As(err, &validation) && !errors.As(err, &problems) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrModel) && !errors.Is(err, ErrRevision) && !errors.Is(err, ErrRoutingManaged) {
			event.Result, event.Reason = audit.Failed, "endpoint change failed"
			_ = audit.Record(ctx, s.DB, event)
			return nil, ErrStorage
		}
		_ = audit.Record(ctx, s.DB, event)
		return nil, err
	}
	publish()
	return s.view(ctx)
}
