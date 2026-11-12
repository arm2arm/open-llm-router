// Package config loads and checks the router's YAML configuration.
//
// The file is read strictly: an unknown key, a reference to an environment
// variable that is not set, or a value that fails validation stops the load,
// and the error lists every problem found. A setting the router does not
// implement is therefore rejected rather than silently ignored.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones also work in the static runtime image.

	"go.yaml.in/yaml/v4"
)

// Protocols a model can be served with.
const (
	ProtocolOpenAI         = "openai"
	ProtocolAnthropic      = "anthropic"
	DefaultKeyLifetimeDays = 90
)

// Environment variables that carry secrets. Secrets have no key in the config
// file, so they cannot end up in a file that is committed or shared.
const (
	EnvDatabaseURL      = "OLR_DATABASE_URL"
	EnvOIDCClientSecret = "OIDC_PORTAL_CLIENT_SECRET"
	EnvOIDCSyncSecret   = "OIDC_SYNC_CLIENT_SECRET"
)

// Config is the content of the config file plus the secrets from the
// environment.
type Config struct {
	Auth      Auth              `yaml:"auth"`
	Limits    Limits            `yaml:"limits"`
	Retention Retention         `yaml:"retention"`
	OIDC      *OIDC             `yaml:"oidc"` // nil: local portal login only
	Portal    Portal            `yaml:"-"`
	Models    map[string]Model  `yaml:"models"`
	Groups    map[string]Group  `yaml:"groups"`
	Aliases   map[string]string `yaml:"aliases"` // client model name -> model
	// Forwarders configures end-user attribution for trusted forwarder keys.
	Forwarders Forwarders `yaml:"forwarders"`

	DatabaseURL      string `yaml:"-"`
	OIDCClientSecret string `yaml:"-"`
	OIDCSyncSecret   string `yaml:"-"` // client secret of oidc.sync_client_id

	// AuthSources records where each authentication setting came from:
	// SourceFile, SourceEnvironment or SourceDefault, keyed by the names in
	// AuthSettings. Managed settings use it to import and to detect conflicts.
	AuthSources map[string]string `yaml:"-"`
}

// Forwarders lets a key flagged trusted_forwarder (such as Open WebUI's) name
// the end user a call is for. HeaderMap maps a request header to the user
// attribute it carries: id, username or email. UnknownUser is "reject" or
// "forwarder_policy" (serve the call as the forwarder itself).
type Forwarders struct {
	HeaderMap   map[string]string `yaml:"header_map"`
	UnknownUser string            `yaml:"unknown_user"`
}

// Forwarder attributes and unknown-user policies.
const (
	EndUserHeader          = "X-OLR-End-User"
	UnknownUserReject      = "reject"
	UnknownForwarderPolicy = "forwarder_policy"
)

// Auth controls the lifetime of newly issued API keys.
type Auth struct {
	DefaultKeyLifetimeDays int `yaml:"default_key_lifetime_days"`
}

// Retention sets how long usage records are kept. A nightly job deletes older
// rows at RunAt ("HH:MM" in limits.timezone) and once at startup.
type Retention struct {
	UsageDays      int    `yaml:"usage_days"`       // per-request records; default 90
	UsageDailyDays int    `yaml:"usage_daily_days"` // daily totals; default 400
	RunAt          string `yaml:"run_at"`           // default "03:00"
}

// Retention defaults.
const (
	DefaultUsageDays      = 90
	DefaultUsageDailyDays = 400
	// MinUsageDailyDays keeps the current and previous month for quotas.
	MinUsageDailyDays = 62
)

const (
	DefaultMaxBodyBytes          = 4 << 20
	DefaultMaxBodyReaders        = 32
	DefaultMaxBodyReadersPerUser = 4
)

// Limits sets the quota calendar and process-local request upload bounds.
type Limits struct {
	Timezone              string         `yaml:"timezone"`
	Location              *time.Location `yaml:"-"`
	MaxBodyBytes          int64          `yaml:"max_body_bytes"`
	MaxBodyReaders        int            `yaml:"max_body_readers"`
	MaxBodyReadersPerUser int            `yaml:"max_body_readers_per_user"`
}

// OIDC describes the identity provider used for portal login.
type OIDC struct {
	Issuer         string `yaml:"issuer"`
	PortalClientID string `yaml:"portal_client_id"`
	GroupsClaim    string `yaml:"groups_claim"`   // default "groups"
	UsernameClaim  string `yaml:"username_claim"` // default "preferred_username"
	BackchannelURL string `yaml:"-"`
	// Audience enables identity-provider access tokens on /v1/*: a token
	// must name it in aud. Empty: API keys only.
	Audience string `yaml:"audience"`
	// SyncClientID is a confidential client with a read-only service
	// account (Keycloak: realm-management view-users). With its secret in
	// OIDC_SYNC_CLIENT_SECRET, groups and enabled state of users are
	// refreshed every SyncIntervalS seconds (default 900).
	SyncClientID  string `yaml:"sync_client_id"`
	SyncIntervalS int    `yaml:"sync_interval_s"`
	// RequiredClaim, if set, must be in the ID token at portal login: a
	// string claim equal to Value, or an array containing it. Env:
	// OIDC_REQUIRED_CLAIM=name=value.
	RequiredClaim *RequiredClaim `yaml:"required_claim"`
	// DefaultSignIn chooses the primary sign-in option: "keycloak" (the
	// default) or "local". Both stay available.
	DefaultSignIn string `yaml:"default_sign_in"`
	// GroupNames is "leaf" (default: "/org/staff" is "staff") or "path"
	// ("/org/staff"); login, API tokens and group sync all apply it.
	GroupNames string `yaml:"group_names"`
}

// RequiredClaim names a claim and the value a user must have.
type RequiredClaim struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type Portal struct {
	BaseURL      string
	CookieSecure bool
}

// Model is one model name served to clients and the replicas behind it.
type Model struct {
	Protocols     []string `yaml:"protocols" json:"protocols"`
	UpstreamModel string   `yaml:"upstream_model" json:"upstream_model"`
	TimeoutS      int      `yaml:"timeout_s" json:"timeout_s"`
	IdleTimeoutS  int      `yaml:"idle_timeout_s" json:"idle_timeout_s"` // default 30; between response chunks
	// QueueTimeoutS lets a call wait this long for a free replica when
	// every replica is at max_inflight; 0 answers 503 at once.
	QueueTimeoutS int `yaml:"queue_timeout_s" json:"queue_timeout_s"`
	// SessionAffinity sends calls with the same X-Session-Id to the same
	// replica while it is healthy and has capacity (prefix-cache reuse).
	SessionAffinity bool `yaml:"session_affinity" json:"session_affinity"`
	// Priority, if set, is sent to the backend as the request field
	// "priority" (vLLM with --scheduling-policy priority; lower is sooner).
	Priority *Priorities `yaml:"priority" json:"priority"`
	Pool     []Replica   `yaml:"pool" json:"pool"`
}

// Priorities maps key classes to backend priorities; a group's priority is
// added to them.
type Priorities struct {
	Interactive int `yaml:"interactive" json:"interactive"`
	Batch       int `yaml:"batch" json:"batch"`
}

// Replica is one backend server of a model.
type Replica struct {
	URL            string  `yaml:"url" json:"url"`
	HealthPath     string  `yaml:"health_path" json:"health_path"` // default /health; Ollama uses /
	Weight         float64 `yaml:"weight" json:"weight"`           // relative throughput
	MaxInflight    int     `yaml:"max_inflight" json:"max_inflight"`
	BearerTokenEnv string  `yaml:"bearer_token_env" json:"bearer_token_env"`
	BearerToken    string  `yaml:"-" json:"-"` // resolved from BearerTokenEnv, never written to YAML
}

// Group is the policy for the members of one identity-provider group. A limit
// that is nil is not enforced.
type Group struct {
	Models         []string `yaml:"models"` // model names, or "*" for all
	TokensPerDay   *int64   `yaml:"tokens_per_day"`
	TokensPerMonth *int64   `yaml:"tokens_per_month"`
	RPM            *int     `yaml:"rpm"`
	TPM            *int64   `yaml:"tpm"`            // tokens per minute, a token bucket
	MaxConcurrent  *int     `yaml:"max_concurrent"` // interactive keys
	// MaxConcurrentBatch limits batch keys, counted apart from interactive
	// ones; omitted, batch keys get max_concurrent.
	MaxConcurrentBatch *int `yaml:"max_concurrent_batch"`
	// StrictQuota reserves max_tokens plus the request size before a call,
	// so concurrent calls cannot exceed tokens_per_day or tokens_per_month.
	StrictQuota bool `yaml:"strict_quota"`
	// Admin grants the portal's admin pages to the group's members.
	Admin bool `yaml:"admin"`
	// Priority is added to the key class's priority (lower is sooner), for
	// the router's queue and the backend's scheduler.
	Priority int `yaml:"priority"`
}

// Problems lists everything found wrong with a config file in one pass.
type Problems []string

// Error puts each problem on its own indented line, to be printed after a
// "config file x:" prefix.
func (p Problems) Error() string {
	return "\n  " + strings.Join(p, "\n  ")
}

// Load reads the config file at path, takes ${VAR} references and the secrets
// from the process environment, and returns the checked configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("config file %s:%w", path, err)
	}
	cfg.DatabaseURL = os.Getenv(EnvDatabaseURL)
	cfg.OIDCClientSecret = os.Getenv(EnvOIDCClientSecret)
	cfg.OIDCSyncSecret = os.Getenv(EnvOIDCSyncSecret)
	return cfg, nil
}

// Parse checks and decodes the YAML in data. lookup resolves ${VAR}
// references; it has the signature of os.LookupEnv. The error, if any, is of
// type Problems.
func Parse(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, Problems{"the file is empty"}
	}
	var doc yaml.Node
	if err := yaml.Load(data, &doc); err != nil {
		return nil, yamlProblems(err, data)
	}
	var problems Problems
	expandNode(&doc, lookup, &problems)
	if len(problems) > 0 {
		return nil, problems
	}

	cfg := Config{Auth: Auth{DefaultKeyLifetimeDays: DefaultKeyLifetimeDays}, Limits: Limits{
		Timezone: "UTC", MaxBodyBytes: DefaultMaxBodyBytes,
		MaxBodyReaders: DefaultMaxBodyReaders, MaxBodyReadersPerUser: DefaultMaxBodyReadersPerUser},
		Forwarders: Forwarders{UnknownUser: UnknownUserReject},
		Retention:  Retention{UsageDays: DefaultUsageDays, UsageDailyDays: DefaultUsageDailyDays, RunAt: "03:00"}}
	if err := doc.Load(&cfg, yaml.WithKnownFields()); err != nil {
		return nil, yamlProblems(err, data)
	}
	cfg.Portal = Portal{BaseURL: "http://localhost:8080", CookieSecure: true}
	if issuer, ok := lookup("OIDC_ISSUER"); ok {
		if cfg.OIDC == nil {
			cfg.OIDC = &OIDC{PortalClientID: "olr-portal"}
		}
		cfg.OIDC.Issuer = issuer
	}
	if cfg.OIDC != nil {
		for name, target := range map[string]*string{"OIDC_AUDIENCE": &cfg.OIDC.Audience, "OIDC_SYNC_CLIENT_ID": &cfg.OIDC.SyncClientID, "OIDC_PORTAL_CLIENT_ID": &cfg.OIDC.PortalClientID, "OIDC_GROUPS_CLAIM": &cfg.OIDC.GroupsClaim, "OIDC_USERNAME_CLAIM": &cfg.OIDC.UsernameClaim, "OIDC_BACKCHANNEL_URL": &cfg.OIDC.BackchannelURL} {
			if value, ok := lookup(name); ok {
				*target = value
			}
		}
	}
	if value, ok := lookup("OIDC_PORTAL_CLIENT_SECRET"); ok {
		cfg.OIDCClientSecret = value
	}
	if value, ok := lookup("OIDC_REQUIRED_CLAIM"); ok && cfg.OIDC != nil {
		if value == "" {
			cfg.OIDC.RequiredClaim = nil
		} else if name, want, found := strings.Cut(value, "="); found {
			cfg.OIDC.RequiredClaim = &RequiredClaim{Name: name, Value: want}
		} else {
			problems = append(problems, "OIDC_REQUIRED_CLAIM: must be name=value")
		}
	}
	if value, ok := lookup(EnvOIDCSyncSecret); ok {
		cfg.OIDCSyncSecret = value
	}
	if value, ok := lookup("PORTAL_BASE_URL"); ok {
		cfg.Portal.BaseURL = strings.TrimSuffix(value, "/")
	}
	if value, ok := lookup("PORTAL_COOKIE_SECURE"); ok {
		secure, err := strconv.ParseBool(value)
		if err != nil {
			problems = append(problems, "PORTAL_COOKIE_SECURE: must be true or false")
		} else {
			cfg.Portal.CookieSecure = secure
		}
	}
	if cfg.OIDC != nil {
		if cfg.OIDC.GroupsClaim == "" {
			cfg.OIDC.GroupsClaim = "groups"
		}
		if cfg.OIDC.UsernameClaim == "" {
			cfg.OIDC.UsernameClaim = "preferred_username"
		}
		if cfg.OIDC.SyncClientID == "" {
			cfg.OIDC.SyncClientID = "olr-sync"
		}
		if cfg.OIDC.SyncIntervalS == 0 {
			cfg.OIDC.SyncIntervalS = 900
		}
		if cfg.OIDC.DefaultSignIn == "" {
			cfg.OIDC.DefaultSignIn = "keycloak"
		}
		if cfg.OIDC.GroupNames == "" {
			cfg.OIDC.GroupNames = "leaf"
		}
	}
	for name, model := range cfg.Models {
		if model.IdleTimeoutS == 0 {
			model.IdleTimeoutS = 30
		}
		for i := range model.Pool {
			r := &model.Pool[i]
			if r.BearerTokenEnv == "" {
				continue
			}
			token, ok := lookup(r.BearerTokenEnv)
			if !isVarName(r.BearerTokenEnv) || !ok || token == "" || strings.ContainsAny(token, "\r\n") {
				problems = append(problems, fmt.Sprintf("models.%s.pool[%d].bearer_token_env: must name a set, nonempty environment variable containing a single-line token", name, i))
			} else {
				r.BearerToken = token
			}
		}
		cfg.Models[name] = model
	}
	if len(problems) > 0 {
		return nil, problems
	}
	cfg.AuthSources = authSources(&doc, lookup, cfg.OIDC != nil)
	// Header names match without regard to case; X-OLR-End-User always works
	// and names a username unless the map says otherwise.
	headers := map[string]string{http.CanonicalHeaderKey(EndUserHeader): "username"}
	for header, attribute := range cfg.Forwarders.HeaderMap {
		headers[http.CanonicalHeaderKey(header)] = attribute
	}
	cfg.Forwarders.HeaderMap = headers
	if problems = cfg.validate(); len(problems) > 0 {
		return nil, problems
	}
	location, err := time.LoadLocation(cfg.Limits.Timezone)
	if err != nil || cfg.Limits.Timezone == "" || cfg.Limits.Timezone == "Local" {
		return nil, Problems{"limits.timezone: must be an explicit IANA time zone, such as UTC or Europe/Berlin"}
	}
	cfg.Limits.Location = location
	return &cfg, nil
}

// yamlProblems turns an error of the yaml package into Problems, one
// "line N: ..." entry per error.
func yamlProblems(err error, data []byte) error {
	var list []*yaml.LoadError
	var many *yaml.LoadErrors
	var one *yaml.LoadError
	switch {
	case errors.As(err, &many):
		list = many.Errors
	case errors.As(err, &one):
		list = []*yaml.LoadError{one}
	default:
		return Problems{err.Error()}
	}
	lines := bytes.Split(data, []byte("\n"))
	var problems Problems
	for _, e := range list {
		msg := e.Message
		if e.ContextMsg != "" {
			msg += " (" + e.ContextMsg + ")"
		}
		if e.Mark.Line == 0 {
			problems = append(problems, msg)
			continue
		}
		// An unquoted ${NAME} inside {...} or [...] is not valid YAML, and
		// the parser's message does not point at the braces.
		syntax := e.Stage == yaml.ScannerStage || e.Stage == yaml.ParserStage
		if syntax && e.Mark.Line <= len(lines) && bytes.Contains(lines[e.Mark.Line-1], []byte("${")) {
			msg += `; inside {...} or [...] a reference must be quoted: "${NAME}"`
		}
		problems = append(problems, fmt.Sprintf("line %d: %s", e.Mark.Line, msg))
	}
	return problems
}

// expandNode replaces ${VAR} references in every scalar below n, keys
// included. It works on the parsed document rather than on the file's text,
// so comments are left alone and a value cannot change the file's structure.
func expandNode(n *yaml.Node, lookup func(string) (string, bool), problems *Problems) {
	for _, child := range n.Content {
		expandNode(child, lookup, problems)
	}
	if n.Kind != yaml.ScalarNode || !strings.Contains(n.Value, "${") {
		return
	}
	value, errs := expand(n.Value, lookup)
	for _, e := range errs {
		*problems = append(*problems, fmt.Sprintf("line %d: %s", n.Line, e))
	}
	n.Value = value

	// The parser has already typed an unquoted "${N}" as a string. Clearing
	// the tag lets the decoder type the expanded text, so that
	// "max_inflight: ${N}" can become a number. Quoted values stay strings.
	const quotedOrBlock = yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle
	if n.Style&(quotedOrBlock|yaml.TaggedStyle) == 0 {
		n.Tag = ""
	}
}

// expand replaces each ${NAME} in s with the value lookup returns for NAME.
func expand(s string, lookup func(string) (string, bool)) (string, []string) {
	var out strings.Builder
	var errs []string
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			out.WriteString(s)
			return out.String(), errs
		}
		out.WriteString(s[:start])
		length := strings.IndexByte(s[start:], '}')
		if length < 0 {
			return out.String(), append(errs, fmt.Sprintf("%q: \"${\" without a closing \"}\"", s[start:]))
		}
		ref := s[start : start+length+1]
		name := ref[2 : len(ref)-1]
		switch value, ok := lookup(name); {
		case !isVarName(name):
			errs = append(errs, fmt.Sprintf("%s: only the form ${NAME} is supported", ref))
		case !ok:
			errs = append(errs, fmt.Sprintf("environment variable %s is not set", name))
		default:
			out.WriteString(value)
		}
		s = s[start+length+1:]
	}
}

func isVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		letter := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (c *Config) validate() Problems {
	var p Problems
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if c.Limits.MaxBodyBytes <= 0 {
		add("limits.max_body_bytes: must be positive")
	}
	if c.Limits.MaxBodyReaders <= 0 {
		add("limits.max_body_readers: must be positive")
	}
	if c.Limits.MaxBodyReadersPerUser <= 0 {
		add("limits.max_body_readers_per_user: must be positive")
	}
	if c.Auth.DefaultKeyLifetimeDays <= 0 || c.Auth.DefaultKeyLifetimeDays > 36500 {
		add("auth.default_key_lifetime_days: must be between 1 and 36500")
	}
	if c.Forwarders.UnknownUser != UnknownUserReject && c.Forwarders.UnknownUser != UnknownForwarderPolicy {
		add("forwarders.unknown_user: must be %q or %q", UnknownUserReject, UnknownForwarderPolicy)
	}
	for _, header := range slices.Sorted(maps.Keys(c.Forwarders.HeaderMap)) {
		if attribute := c.Forwarders.HeaderMap[header]; attribute != "id" && attribute != "username" && attribute != "email" {
			add("forwarders.header_map.%s: must be id, username or email", header)
		}
		if header == "" || strings.ContainsAny(header, " :\r\n") {
			add("forwarders.header_map: %q is not a header name", header)
		}
	}
	if c.Retention.UsageDays < 1 || c.Retention.UsageDays > 36500 {
		add("retention.usage_days: must be between 1 and 36500")
	}
	if c.Retention.UsageDailyDays < MinUsageDailyDays || c.Retention.UsageDailyDays > 36500 {
		add("retention.usage_daily_days: must be between %d (quotas need the current month) and 36500", MinUsageDailyDays)
	}
	if _, err := time.Parse("15:04", c.Retention.RunAt); err != nil || len(c.Retention.RunAt) != 5 {
		add("retention.run_at: must be a time of day as HH:MM, such as 03:00")
	}

	if c.OIDC != nil {
		if !isIssuerURL(c.OIDC.Issuer) {
			add("oidc.issuer: must be an http or https URL without credentials, query or fragment")
		}
		if c.OIDC.PortalClientID == "" {
			add("oidc.portal_client_id: must be set")
		}
		if rc := c.OIDC.RequiredClaim; rc != nil && (rc.Name == "" || rc.Value == "") {
			add("oidc.required_claim: name and value must both be set")
		}
		if c.OIDC.SyncIntervalS < 60 {
			add("oidc.sync_interval_s: must be at least 60")
		}
		if c.OIDC.BackchannelURL != "" && !isIssuerURL(c.OIDC.BackchannelURL) {
			add("OIDC_BACKCHANNEL_URL: must be an http or https URL without credentials, query or fragment")
		}
		if c.OIDC.DefaultSignIn != "" && c.OIDC.DefaultSignIn != "keycloak" && c.OIDC.DefaultSignIn != "local" {
			add("oidc.default_sign_in: must be keycloak or local")
		}
		if c.OIDC.GroupNames != "" && c.OIDC.GroupNames != "leaf" && c.OIDC.GroupNames != "path" {
			add("oidc.group_names: must be leaf or path")
		}
	}
	if !isBackendURL(c.Portal.BaseURL) {
		add("PORTAL_BASE_URL: must be an http or https origin without credentials, path, query or fragment")
	}

	if len(c.Models) == 0 {
		add("models: at least one model is required")
	}
	for _, name := range slices.Sorted(maps.Keys(c.Models)) {
		m, at := c.Models[name], "models."+name
		if name == "" || name == "*" {
			add("models: %q cannot be used as a model name", name)
		}
		if len(m.Protocols) == 0 {
			add("%s.protocols: list %q, %q or both", at, ProtocolOpenAI, ProtocolAnthropic)
		}
		for _, proto := range m.Protocols {
			if proto != ProtocolOpenAI && proto != ProtocolAnthropic {
				add("%s.protocols: unknown protocol %q (known: %q, %q)", at, proto, ProtocolOpenAI, ProtocolAnthropic)
			}
		}
		if m.TimeoutS <= 0 {
			add("%s.timeout_s: must be greater than 0", at)
		}
		if m.IdleTimeoutS <= 0 {
			add("%s.idle_timeout_s: must be greater than 0", at)
		}
		if m.QueueTimeoutS < 0 || m.QueueTimeoutS > m.TimeoutS {
			add("%s.queue_timeout_s: must be between 0 and timeout_s", at)
		}
		if len(m.Pool) == 0 {
			add("%s.pool: at least one replica is required", at)
		}
		seen := make(map[string]bool)
		for i, r := range m.Pool {
			at := fmt.Sprintf("%s.pool[%d]", at, i)
			switch {
			case !isBackendURL(r.URL):
				add("%s.url: must be an http or https URL, got %q", at, r.URL)
			case seen[r.URL]:
				add("%s.url: %s is listed twice in this pool", at, r.URL)
			}
			seen[r.URL] = true
			if r.HealthPath != "" {
				u, err := url.ParseRequestURI(r.HealthPath)
				if err != nil || !strings.HasPrefix(r.HealthPath, "/") || strings.HasPrefix(r.HealthPath, "//") || strings.ContainsAny(r.HealthPath, "?#") || u.IsAbs() {
					add("%s.health_path: must be an absolute URL path without a query or fragment", at)
				}
			}
			if r.Weight <= 0 || math.IsNaN(r.Weight) || math.IsInf(r.Weight, 0) {
				add("%s.weight: must be greater than 0", at)
			}
			if r.MaxInflight <= 0 {
				add("%s.max_inflight: must be greater than 0", at)
			}
		}
	}

	for _, alias := range slices.Sorted(maps.Keys(c.Aliases)) {
		target := c.Aliases[alias]
		if _, isModel := c.Models[alias]; isModel || alias == "" || alias == "*" {
			add("aliases: %q cannot be used as an alias (it is empty, \"*\" or the name of a model)", alias)
		}
		if _, ok := c.Models[target]; !ok {
			add("aliases.%s: %q is not a configured model", alias, target)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(c.Groups)) {
		g, at := c.Groups[name], "groups."+name
		if len(g.Models) == 0 {
			add("%s.models: list at least one model, or \"*\" for all", at)
		}
		for _, model := range g.Models {
			if _, ok := c.Models[model]; !ok && model != "*" {
				add("%s.models: %q is not a configured model", at, model)
			}
		}
		// A limit of 0 would read as "no limit" to some and "no access" to
		// others, so it is refused: leave the key out for no limit.
		limits := []struct {
			key string
			set bool
			val int64
		}{
			{"tokens_per_day", g.TokensPerDay != nil, deref(g.TokensPerDay)},
			{"tokens_per_month", g.TokensPerMonth != nil, deref(g.TokensPerMonth)},
			{"rpm", g.RPM != nil, int64(deref(g.RPM))},
			{"tpm", g.TPM != nil, deref(g.TPM)},
			{"max_concurrent", g.MaxConcurrent != nil, int64(deref(g.MaxConcurrent))},
			{"max_concurrent_batch", g.MaxConcurrentBatch != nil, int64(deref(g.MaxConcurrentBatch))},
		}
		for _, l := range limits {
			if l.set && l.val <= 0 {
				add("%s.%s: must be greater than 0; leave it out for no limit", at, l.key)
			}
		}
	}
	return p
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func isIssuerURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && isHTTPURL(s) && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// Backend URLs name origins; paths and queries come exclusively from the caller.
func isBackendURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && isHTTPURL(s) && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
