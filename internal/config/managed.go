package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"go.yaml.in/yaml/v4"
)

// WithModels validates an immutable routing copy against the file-owned group
// and alias policies. Resolved tokens are reconstructed, never accepted in JSON.
func WithModels(base *Config, models map[string]Model, lookup func(string) (string, bool)) (*Config, error) {
	return WithRouting(base, models, base.Aliases, lookup)
}

// WithRouting validates an immutable copy of base with these models and
// aliases. Aliases and group policies must name configured models, so a model
// still in use cannot be removed. Backend credentials are resolved through
// lookup from bearer_token_env references.
func WithRouting(base *Config, models map[string]Model, aliases map[string]string, lookup func(string) (string, bool)) (*Config, error) {
	return WithPolicies(base, models, aliases, base.Groups, lookup)
}

// WithPolicies copies and validates models, aliases and group policies together.
// No model or policy from the previous revision can leak into the replacement.
func WithPolicies(base *Config, models map[string]Model, aliases map[string]string, groups map[string]Group, lookup func(string) (string, bool)) (*Config, error) {
	data, err := json.Marshal(models)
	if err != nil {
		return nil, Problems{"invalid model configuration"}
	}
	var copied map[string]Model
	if json.Unmarshal(data, &copied) != nil {
		return nil, Problems{"invalid model configuration"}
	}
	cfg := *base
	cfg.Groups = nil
	groupData, err := json.Marshal(groups)
	if err != nil {
		return nil, Problems{"invalid group policies"}
	}
	if json.Unmarshal(groupData, &cfg.Groups) != nil {
		return nil, Problems{"invalid group policies"}
	}
	cfg.Models = copied
	cfg.Aliases = maps.Clone(aliases)
	if cfg.Aliases == nil {
		cfg.Aliases = map[string]string{}
	}
	var problems Problems
	for name, model := range cfg.Models {
		if model.IdleTimeoutS == 0 {
			model.IdleTimeoutS = 30
		}
		for i := range model.Pool {
			r := &model.Pool[i]
			if !isBackendURL(r.URL) {
				problems = append(problems, fmt.Sprintf("models.%s.pool[%d]: backend URL must be an HTTP(S) origin without credentials, path, query or fragment", name, i))
				continue
			}
			r.URL = strings.TrimRight(r.URL, "/")
			if r.BearerTokenEnv != "" {
				token, ok := lookup(r.BearerTokenEnv)
				if !isVarName(r.BearerTokenEnv) || !ok || token == "" || strings.ContainsAny(token, "\r\n") {
					problems = append(problems, fmt.Sprintf("models.%s.pool[%d]: choose an available backend credential reference", name, i))
				} else {
					r.BearerToken = token
				}
			}
		}
		cfg.Models[name] = model
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if problems = cfg.validate(); len(problems) > 0 {
		return nil, problems
	}
	return &cfg, nil
}

// Sources of a setting value.
const (
	SourceDefault     = "default"
	SourceFile        = "file"
	SourceEnvironment = "environment"
)

// AuthSetting names one authentication setting with its YAML key under oidc
// and its environment variable; either can be empty.
type AuthSetting struct{ Name, YAML, Env string }

// AuthSettings lists the authentication settings in display order. "enabled"
// is derived: an oidc section or a nonempty OIDC_ISSUER turns SSO on.
var AuthSettings = []AuthSetting{
	{"enabled", "", "OIDC_ISSUER"},
	{"default_sign_in", "default_sign_in", ""},
	{"issuer", "issuer", "OIDC_ISSUER"},
	{"backchannel_url", "", "OIDC_BACKCHANNEL_URL"},
	{"portal_client_id", "portal_client_id", "OIDC_PORTAL_CLIENT_ID"},
	{"portal_client_secret", "", EnvOIDCClientSecret},
	{"audience", "audience", "OIDC_AUDIENCE"},
	{"groups_claim", "groups_claim", "OIDC_GROUPS_CLAIM"},
	{"group_names", "group_names", ""},
	{"username_claim", "username_claim", "OIDC_USERNAME_CLAIM"},
	{"required_claim", "required_claim", "OIDC_REQUIRED_CLAIM"},
	{"sync_client_id", "sync_client_id", "OIDC_SYNC_CLIENT_ID"},
	{"sync_client_secret", "", EnvOIDCSyncSecret},
	{"sync_interval_s", "sync_interval_s", ""},
}

// authSources mirrors how Parse applies values: environment variables other
// than the client secrets only take effect once SSO is enabled.
func authSources(doc *yaml.Node, lookup func(string) (string, bool), enabled bool) map[string]string {
	keys := map[string]bool{}
	section := false
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		root := doc.Content[0].Content
		for i := 0; i+1 < len(root); i += 2 {
			if root[i].Value == "oidc" && root[i+1].Kind == yaml.MappingNode {
				section = true
				for j := 0; j+1 < len(root[i+1].Content); j += 2 {
					keys[root[i+1].Content[j].Value] = true
				}
			}
		}
	}
	sources := map[string]string{}
	for _, setting := range AuthSettings {
		source := SourceDefault
		if setting.YAML != "" && keys[setting.YAML] || setting.Name == "enabled" && section {
			source = SourceFile
		}
		if setting.Env != "" {
			_, set := lookup(setting.Env)
			secret := setting.Env == EnvOIDCClientSecret || setting.Env == EnvOIDCSyncSecret
			if set && (enabled || secret) {
				source = SourceEnvironment
			}
		}
		sources[setting.Name] = source
	}
	return sources
}

// WithAuthentication returns a validated copy of base using the given SSO
// connection (nil: local login only) and client secrets.
func WithAuthentication(base *Config, oidc *OIDC, portalSecret, syncSecret string) (*Config, error) {
	cfg := *base
	cfg.OIDC = nil
	if oidc != nil {
		copied := *oidc
		if oidc.RequiredClaim != nil {
			claim := *oidc.RequiredClaim
			copied.RequiredClaim = &claim
		}
		cfg.OIDC = &copied
	}
	cfg.OIDCClientSecret, cfg.OIDCSyncSecret = portalSecret, syncSecret
	if problems := cfg.validate(); len(problems) > 0 {
		return nil, problems
	}
	return &cfg, nil
}
