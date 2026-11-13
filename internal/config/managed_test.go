package config

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

func TestManagedModelsAreValidatedCopiedAndRedacted(t *testing.T) {
	base, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	models := maps.Clone(base.Models)
	model := models["chat-model"]
	model.Pool = append([]Replica(nil), model.Pool...)
	model.UpstreamModel = "backend-model"
	model.Pool[0].BearerTokenEnv = "GPU_TOKEN"
	model.Pool[0].BearerToken = "untrusted-in-memory-value"
	models["chat-model"] = model
	next, err := WithModels(base, models, lookupIn(map[string]string{"GPU_TOKEN": "resolved-backend-secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Models["chat-model"].Pool[0].BearerToken != "resolved-backend-secret" {
		t.Fatal("credential was not resolved from its allowlisted reference")
	}
	if base.Models["chat-model"].UpstreamModel != "" || base.Models["chat-model"].Pool[0].BearerToken != "" {
		t.Fatal("active file snapshot mutated")
	}
	encoded, _ := json.Marshal(next.Models)
	if strings.Contains(string(encoded), "resolved-backend-secret") || strings.Contains(string(encoded), "untrusted-in-memory-value") {
		t.Fatal("serialized configuration leaked a token")
	}
	model.Pool[0].URL = "http://user:private-token@backend"
	models["chat-model"] = model
	if _, err := WithModels(base, models, lookupIn(nil)); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("credential URL validation leaked its value")
	}
}

func TestAuthSourcesFollowParse(t *testing.T) {
	file := `
oidc:
  issuer: https://idp.example/realms/a
  portal_client_id: olr-portal
models:
  m:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: "http://backend:8000", weight: 1, max_inflight: 1}
`
	env := map[string]string{"OIDC_AUDIENCE": "olr-api", EnvOIDCClientSecret: "s"}
	cfg, err := Parse([]byte(file), lookupIn(env))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"enabled": SourceFile, "issuer": SourceFile, "portal_client_id": SourceFile, "audience": SourceEnvironment,
		"portal_client_secret": SourceEnvironment, "groups_claim": SourceDefault, "sync_client_secret": SourceDefault, "sync_interval_s": SourceDefault}
	for name, source := range want {
		if cfg.AuthSources[name] != source {
			t.Fatalf("%s: %s, want %s", name, cfg.AuthSources[name], source)
		}
	}
	if len(cfg.AuthSources) != len(AuthSettings) {
		t.Fatal("every setting needs a source")
	}
	// Without SSO, only the client secrets are read from the environment.
	local, err := Parse([]byte(file[strings.Index(file, "models:"):]), lookupIn(env))
	if err != nil {
		t.Fatal(err)
	}
	if local.AuthSources["audience"] != SourceDefault || local.AuthSources["enabled"] != SourceDefault || local.AuthSources["portal_client_secret"] != SourceEnvironment {
		t.Fatal(local.AuthSources)
	}
	issuer, err := Parse([]byte(file[strings.Index(file, "models:"):]), lookupIn(map[string]string{"OIDC_ISSUER": "https://idp.example/realms/b"}))
	if err != nil || issuer.AuthSources["enabled"] != SourceEnvironment || issuer.AuthSources["issuer"] != SourceEnvironment {
		t.Fatal("OIDC_ISSUER enables SSO from the environment", err)
	}
}

func TestPoliciesCopyAndValidateTheWholeRevision(t *testing.T) {
	base, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	one := 7
	groups := map[string]Group{"managed": {Models: []string{"chat-model"}, RPM: &one}}
	next, err := WithPolicies(base, base.Models, base.Aliases, groups, lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	next.Groups["managed"].Models[0] = "changed"
	*next.Groups["managed"].RPM = 9
	if groups["managed"].Models[0] != "chat-model" || *groups["managed"].RPM != 7 {
		t.Fatal("revision aliases input policies")
	}
	if _, ok := base.Groups["managed"]; ok {
		t.Fatal("revision mutated deployment baseline")
	}
	groups["managed"] = Group{Models: []string{"nonexistent"}}
	if _, err = WithPolicies(base, base.Models, base.Aliases, groups, lookupIn(validEnv)); err == nil {
		t.Fatal("accepted unknown model grant")
	}
}
