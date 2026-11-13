package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// Limits on Admin-managed routing documents.
const (
	maxModels   = 256
	maxReplicas = 32
	maxAliases  = 256
)

// Routing is the managed set of logical models with their endpoint pools, and
// the aliases clients may use for them. Endpoint credentials are references to
// allowed environment variables (bearer_token_env); resolved tokens are never
// part of the document. Group policies activate in the same revision.
type Routing struct {
	Models  map[string]config.Model `json:"models"`
	Aliases map[string]string       `json:"aliases"`
	Groups  map[string]config.Group `json:"groups"`
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
	*r = Routing{Models: models, Aliases: maps.Clone(current.Aliases), Groups: groups}
	if r.Aliases == nil {
		r.Aliases = map[string]string{}
	}
}

func (r *Routing) problems() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if len(r.Models) == 0 || len(r.Models) > maxModels {
		add("models: list between 1 and %d models", maxModels)
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

func (*Routing) secrets() []secretBinding { return nil }

// apply validates the routing against base's group policies and resolves
// endpoint credentials; config problems name keys, never credential values.
func (r *Routing) apply(base *config.Config, in applyInput) (*config.Config, error) {
	groups := r.Groups
	if groups == nil { // pre-policy revisions used deployment policies
		groups = base.Groups
	}
	cfg, err := config.WithPolicies(base, r.Models, r.Aliases, groups, in.tokens)
	if err != nil {
		var problems config.Problems
		if errors.As(err, &problems) {
			return nil, &ValidationError{problems}
		}
		return nil, &ValidationError{[]string{"the routing does not form a valid configuration"}}
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
	return values
}

func (*Routing) settingNames() []string { return []string{"models", "aliases", "groups"} }

func (*Routing) fileSources(*config.Config) map[string]string {
	return map[string]string{"models": config.SourceFile, "aliases": config.SourceFile, "groups": config.SourceFile}
}

// checksPassed: activation probes the changed endpoints itself.
func (*Routing) checksPassed(*Checks) bool { return true }

// fallback refuses to start: serving no or stale models silently is worse
// than an explicit startup error with a rollback command.
func (*Routing) fallback(*config.Config) (*config.Config, string, bool) { return nil, "", false }
