package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// Limits on Admin-managed routing documents.
const (
	maxModels   = 256
	maxReplicas = 32
	maxAliases  = 256
)

// Routing is the managed set of logical models with their endpoint pools, and
// the aliases clients may use for them. Backends carry encrypted API key
// references; legacy endpoints may reference allowed environment variables.
// Resolved tokens are never part of the document. Group policies activate in
// the same revision.
type Routing struct {
	Models         map[string]config.Model       `json:"models"`
	Aliases        map[string]string             `json:"aliases"`
	Groups         map[string]config.Group       `json:"groups"`
	Backends       map[string]*Backend           `json:"backends,omitempty"`
	BackendPresets map[string]config.BackendRate `json:"backend_presets,omitempty"`
}

func (*Routing) domainName() string { return DomainRouting }

func (r *Routing) decode(data []byte) error {
	var doc Routing
	if err := strictDecode(data, &doc, "routing"); err != nil {
		return err
	}
	if doc.Models == nil {
		doc.Models = map[string]config.Model{}
	}
	if doc.Aliases == nil {
		doc.Aliases = map[string]string{}
	}
	if doc.BackendPresets == nil {
		doc.BackendPresets = config.DefaultBackendPresets()
	}
	// Persist the same origins used for discovery and activation. Invalid
	// inputs are left intact for redacted validation errors below.
	for _, backend := range doc.Backends {
		if backend != nil {
			if origin, err := NormalizeBackendURL(backend.URL); err == nil {
				backend.URL = origin
			}
		}
	}
	for name, model := range doc.Models {
		for i := range model.Pool {
			if model.Pool[i].Backend == "" {
				if origin, err := NormalizeBackendURL(model.Pool[i].URL); err == nil {
					model.Pool[i].URL = origin
				}
			}
		}
		doc.Models[name] = model
	}
	*r = doc
	return nil
}

// baseline copies the routing in use, including Admin endpoint revisions.
func (r *Routing) baseline(current *config.Config) {
	data, _ := json.Marshal(current.Models)
	models := map[string]config.Model{}
	_ = json.Unmarshal(data, &models)
	groupData, _ := json.Marshal(current.Groups)
	groups := map[string]config.Group{}
	_ = json.Unmarshal(groupData, &groups)
	*r = Routing{Models: models, Aliases: maps.Clone(current.Aliases), Groups: groups, BackendPresets: config.DefaultBackendPresets()}
	if r.Aliases == nil {
		r.Aliases = map[string]string{}
	}
}

func (r *Routing) problems() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	p = append(p, r.backendProblems()...)
	if len(r.Models) > maxModels {
		add("models: list at most %d models", maxModels)
	}
	for _, name := range slices.Sorted(maps.Keys(r.Models)) {
		if name == "" || name == "*" || !plainText(name, 128) {
			add("models: a model name must be 1 to 128 bytes of text, not \"*\"")
			continue
		}
		if len(r.Models[name].Pool) > maxReplicas {
			add("models.%s.pool: at most %d replicas", name, maxReplicas)
		}
	}
	if len(r.Aliases) > maxAliases {
		add("aliases: at most %d aliases", maxAliases)
	}
	for _, alias := range slices.Sorted(maps.Keys(r.Aliases)) {
		if !plainText(alias, 128) || !plainText(r.Aliases[alias], 128) {
			add("aliases: alias names and targets must be up to 128 bytes of text")
		}
	}
	if len(r.Groups) > 256 {
		add("groups: at most 256 groups")
	}
	for _, name := range slices.Sorted(maps.Keys(r.Groups)) {
		if !plainText(name, 128) {
			add("groups: names must be 1 to 128 bytes of text")
		}
		if len(r.Groups[name].Models) > maxModels {
			add("groups.%s.models: at most %d model grants", name, maxModels)
		}
	}
	return p
}

func (r *Routing) secrets() []secretBinding {
	var bindings []secretBinding
	for _, id := range slices.Sorted(maps.Keys(r.Backends)) {
		if backend := r.Backends[id]; backend != nil {
			bindings = append(bindings, secretBinding{name: backend.SecretName(id), ref: &backend.APIKey})
		}
	}
	return bindings
}

// apply validates the routing against base's group policies and resolves
// endpoint credentials; config problems name keys, never credential values.
func (r *Routing) apply(base *config.Config, in applyInput) (*config.Config, error) {
	groups := r.Groups
	if groups == nil { // pre-policy revisions used deployment policies
		groups = base.Groups
	}
	models := maps.Clone(r.Models)
	for name, model := range models {
		model.Pool = slices.Clone(model.Pool)
		for i := range model.Pool {
			replica := &model.Pool[i]
			if replica.Backend == "" {
				continue
			}
			backend := r.Backends[replica.Backend]
			if backend == nil {
				return nil, &ValidationError{[]string{"models." + name + ": choose an existing backend"}}
			}
			replica.URL, replica.PrivateNetwork = backend.URL, backend.PrivateNetwork
			if replica.HealthPath == "" {
				replica.HealthPath = backend.HealthPath
			}
		}
		models[name] = model
	}
	lookup := func(name string) (string, bool) {
		if id, ok := strings.CutPrefix(name, "backend:"); ok {
			backend := r.Backends[id]
			if backend == nil {
				return "", false
			}
			return in.secrets[backend.SecretName(id)], true
		}
		if in.tokens == nil {
			return "", false
		}
		return in.tokens(name)
	}
	cfg, err := config.WithPolicies(base, models, r.Aliases, groups, lookup)
	if err != nil {
		var problems config.Problems
		if errors.As(err, &problems) {
			return nil, &ValidationError{problems}
		}
		return nil, &ValidationError{[]string{"the routing does not form a valid configuration"}}
	}
	cfg.Backends = map[string]config.BackendPolicy{}
	for id, backend := range r.Backends {
		if backend == nil {
			return nil, &ValidationError{[]string{"backends: missing backend"}}
		}
		preset := r.BackendPresets[backend.Preset]
		cfg.Backends[id] = config.BackendPolicy{Groups: slices.Clone(backend.Groups), Rate: preset}
	}
	return cfg, nil
}

// conflicts reports file models and aliases that differ. They are
// informational: the file must keep at least one model to load, so its
// routing is only the import baseline while routing is managed.
func (r *Routing) conflicts(file *config.Config, _ map[string]string, _ bool) []Conflict {
	var found []Conflict
	encode := func(m config.Model) string { data, _ := json.Marshal(m); return string(data) }
	for _, name := range slices.Sorted(maps.Keys(file.Models)) {
		if managed, ok := r.Models[name]; !ok || encode(managed) != encode(file.Models[name]) {
			found = append(found, Conflict{Setting: "models." + name, Source: config.SourceFile, Where: "models." + name})
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(file.Aliases)) {
		if r.Aliases[alias] != file.Aliases[alias] {
			found = append(found, Conflict{Setting: "aliases." + alias, Source: config.SourceFile, Where: "aliases." + alias})
		}
	}
	return found
}

func (*Routing) conflictsBlock() bool { return false }

// values lists each model and alias, so audit events name what changed.
func (r *Routing) values() map[string]string {
	values := map[string]string{}
	for name, model := range r.Models {
		data, _ := json.Marshal(model)
		values["models."+name] = string(data)
	}
	for alias, target := range r.Aliases {
		values["aliases."+alias] = target
	}
	for name, group := range r.Groups {
		data, _ := json.Marshal(group)
		values["groups."+name] = string(data)
	}
	for id, backend := range r.Backends {
		data, _ := json.Marshal(backend)
		values["backends."+id] = string(data)
	}
	for id, preset := range r.BackendPresets {
		data, _ := json.Marshal(preset)
		values["backend_presets."+id] = string(data)
	}
	return values
}

func (*Routing) settingNames() []string {
	return []string{"models", "aliases", "groups", "backends", "backend_presets"}
}

func (*Routing) fileSources(*config.Config) map[string]string {
	return map[string]string{"models": config.SourceFile, "aliases": config.SourceFile, "groups": config.SourceFile, "backends": config.SourceDefault, "backend_presets": config.SourceDefault}
}

// checksPassed: activation probes the changed endpoints itself.
func (*Routing) checksPassed(*Checks) bool { return true }

// fallback refuses to start: serving no or stale models silently is worse
// than an explicit startup error with a rollback command.
func (*Routing) fallback(*config.Config) (*config.Config, string, bool) { return nil, "", false }
