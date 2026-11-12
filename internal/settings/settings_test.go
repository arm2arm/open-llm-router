package settings

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func testCipher(t *testing.T, fill byte) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	c, err := LoadCipher(func(string) string { return base64.StdEncoding.EncodeToString(key) })
	if err != nil || c == nil || len(c.ID()) != 24 {
		t.Fatal("cipher", err)
	}
	return c
}

func TestCipherBindsSecretsToIDNameAndKey(t *testing.T) {
	if c, err := LoadCipher(func(string) string { return "" }); c != nil || err != nil {
		t.Fatal("an unset key must disable stored secrets without an error")
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := LoadCipher(func(string) string { return bad }); err == nil || strings.Contains(err.Error(), bad) {
			t.Fatal("malformed key accepted or echoed")
		}
	}
	c, other := testCipher(t, 1), testCipher(t, 2)
	id, name := "00000000-0000-4000-8000-000000000001", "authentication.portal_client_secret"
	sealed, err := c.seal(id, name, []byte("client secret value"))
	if err != nil || strings.Contains(string(sealed), "client secret value") {
		t.Fatal("seal", err)
	}
	if plain, err := c.open(id, name, c.ID(), sealed); err != nil || string(plain) != "client secret value" {
		t.Fatal("open", err)
	}
	for _, tc := range []struct {
		cipher        *Cipher
		id, name, kid string
		description   string
	}{
		{c, "00000000-0000-4000-8000-000000000002", name, c.ID(), "another row's id"},
		{c, id, "authentication.sync_client_secret", c.ID(), "another setting"},
		{other, id, name, c.ID(), "another key"},
		{c, id, name, other.ID(), "a different key id"},
		{nil, id, name, c.ID(), "no key"},
	} {
		if _, err := tc.cipher.open(tc.id, tc.name, tc.kid, sealed); !errors.Is(err, errSecretUnavailable) {
			t.Fatal("ciphertext opened with", tc.description)
		}
	}
}

func decodeAuthentication(data []byte) (Authentication, error) {
	var doc Authentication
	err := doc.decode(data)
	return doc, err
}

func TestDecodeIsStrictAndNeverEchoesValues(t *testing.T) {
	doc, err := decodeAuthentication([]byte(`{"enabled":true,"issuer":"https://idp.example/realms/a","portal_client_id":"olr-portal"}`))
	if err != nil || doc.GroupsClaim != "groups" || doc.UsernameClaim != "preferred_username" || doc.SyncClientID != "olr-sync" || doc.SyncIntervalS != 900 {
		t.Fatal("defaults", err, doc)
	}
	for _, input := range []string{
		`{"issuer":"https://idp.example","client_secret":"hunter2-private-value"}`,
		`{"sync_interval_s":"hunter2-private-value"}`,
		`{"issuer":"hunter2-private-value"} {"x":1}`,
		`hunter2-private-value`,
	} {
		_, err := decodeAuthentication([]byte(input))
		var invalid *ValidationError
		if !errors.As(err, &invalid) || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("input %q: %v", input, err)
		}
	}
}

func TestDocumentProblems(t *testing.T) {
	valid := Authentication{Enabled: true, DefaultSignIn: "keycloak", GroupNames: "leaf", Issuer: "https://idp.example/realms/a", PortalClientID: "olr-portal",
		GroupsClaim: "groups", UsernameClaim: "preferred_username", SyncClientID: "olr-sync", SyncIntervalS: 900,
		PortalClientSecret: SecretRef{Env: config.EnvOIDCClientSecret}}
	if p := valid.problems(); len(p) != 0 {
		t.Fatal(p)
	}
	for name, mutate := range map[string]func(*Authentication){
		"issuer":               func(a *Authentication) { a.Issuer = "https://user:pw@idp.example" },
		"portal_client_id":     func(a *Authentication) { a.PortalClientID = "" },
		"backchannel_url":      func(a *Authentication) { a.BackchannelURL = "ftp://idp" },
		"sync_interval_s":      func(a *Authentication) { a.SyncIntervalS = 5 },
		"required_claim":       func(a *Authentication) { a.RequiredClaim = &RequiredClaim{Name: "affiliation"} },
		"audience":             func(a *Authentication) { a.Audience = "line\nbreak" },
		"portal_client_secret": func(a *Authentication) { a.PortalClientSecret = SecretRef{Env: "OLR_DATABASE_URL"} },
		"sync_client_secret":   func(a *Authentication) { a.SyncClientSecret = SecretRef{Env: config.EnvOIDCSyncSecret, Stored: "x"} },
	} {
		doc := valid
		mutate(&doc)
		p := doc.problems()
		if len(p) != 1 || !strings.HasPrefix(p[0], name+":") {
			t.Fatal(name, p)
		}
	}
	// Disabled SSO keeps retained values without requiring them.
	disabled := Authentication{DefaultSignIn: "keycloak", GroupNames: "leaf", SyncIntervalS: 900}
	if p := disabled.problems(); len(p) != 0 {
		t.Fatal("disabled", p)
	}
}

func parse(t *testing.T, yaml string, env map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml+`
models:
  m:
    protocols: [openai]
    timeout_s: 5
    pool:
      - {url: "http://backend:8000", weight: 1, max_inflight: 1}
`), func(name string) (string, bool) { value, ok := env[name]; return value, ok })
	if err != nil {
		t.Fatal(err)
	}
	cfg.OIDCClientSecret, cfg.OIDCSyncSecret = env[config.EnvOIDCClientSecret], env[config.EnvOIDCSyncSecret]
	return cfg
}

func TestImportBaselineReproducesFileConfiguration(t *testing.T) {
	env := map[string]string{"OIDC_ISSUER": "http://keycloak.example/realms/dev", "OIDC_AUDIENCE": "olr-api",
		config.EnvOIDCClientSecret: "portal-secret", "OIDC_REQUIRED_CLAIM": "affiliation=member"}
	file := parse(t, "oidc:\n  portal_client_id: olr-portal\n  sync_interval_s: 120\n", env)
	doc := authenticationFromFile(file)
	if doc.PortalClientSecret != (SecretRef{Env: config.EnvOIDCClientSecret}) || !doc.SyncClientSecret.empty() {
		t.Fatal("secrets must become references to their own variables", doc)
	}
	applied, err := doc.apply(file, applyInput{secrets: map[string]string{"portal_client_secret": "portal-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if *applied.OIDC.RequiredClaim != *file.OIDC.RequiredClaim || applied.OIDCClientSecret != file.OIDCClientSecret {
		t.Fatal("required claim or secret changed")
	}
	got, want := *applied.OIDC, *file.OIDC
	got.RequiredClaim, want.RequiredClaim = nil, nil
	if got != want {
		t.Fatalf("import changed the SSO connection:\n%+v\n%+v", got, want)
	}
	if found := doc.conflicts(file, map[string]string{"portal_client_secret": "portal-secret"}, true); len(found) != 0 {
		t.Fatal("an import conflicts with its own source", found)
	}
	// A local-only file imports as disabled SSO.
	local := parse(t, "", nil)
	if doc := authenticationFromFile(local); doc.Enabled || len(doc.problems()) != 0 {
		t.Fatal("local-only import", doc)
	}
}

func TestConflictsNameSourcesNotValues(t *testing.T) {
	env := map[string]string{"OIDC_ISSUER": "http://old.example/realms/a", config.EnvOIDCClientSecret: "env-secret-value"}
	file := parse(t, "oidc:\n  portal_client_id: file-client\n", env)
	doc := authenticationFromFile(file)
	doc.Issuer, doc.PortalClientID = "http://new.example/realms/b", "managed-client"
	doc.PortalClientSecret = SecretRef{Stored: "00000000-0000-4000-8000-000000000001"}
	found := doc.conflicts(file, map[string]string{"portal_client_secret": "stored-secret-value"}, true)
	var names []string
	for _, conflict := range found {
		names = append(names, conflict.Setting+"@"+conflict.Where)
		for _, value := range []string{"old.example", "new.example", "file-client", "managed-client", "secret-value"} {
			if strings.Contains(conflict.String(), value) {
				t.Fatal("conflict exposed a value", conflict)
			}
		}
	}
	want := []string{"issuer@OIDC_ISSUER", "portal_client_id@oidc.portal_client_id", "portal_client_secret@" + config.EnvOIDCClientSecret}
	if !slices.Equal(names, want) {
		t.Fatal(names)
	}
	// The same stored value as the environment is redundant, not a conflict.
	doc = authenticationFromFile(file)
	doc.PortalClientSecret = SecretRef{Stored: "00000000-0000-4000-8000-000000000001"}
	if found := doc.conflicts(file, map[string]string{"portal_client_secret": "env-secret-value"}, true); len(found) != 0 {
		t.Fatal(found)
	}
	// Unknown stored values are treated as conflicting.
	if found := doc.conflicts(file, map[string]string{}, false); len(found) != 1 {
		t.Fatal(found)
	}
}

func TestRoutingDocument(t *testing.T) {
	file := parse(t, "aliases:\n  default: m\ngroups:\n  staff: {models: [m]}\n", nil)
	var doc Routing
	doc.baseline(file)
	if len(doc.Models) != 1 || doc.Aliases["default"] != "m" || len(doc.problems()) != 0 || len(doc.conflicts(file, nil, true)) != 0 {
		t.Fatal("baseline", doc)
	}
	// A token value can never be submitted: only references are fields.
	if err := doc.decode([]byte(`{"models":{"m":{"pool":[{"url":"http://b:1","BearerToken":"x"}]}}}`)); err == nil {
		t.Fatal("a resolved token field was accepted")
	}
	added := doc
	added.Models = map[string]config.Model{"m": doc.Models["m"], "n": doc.Models["m"]}
	added.Aliases = map[string]string{"default": "n"}
	cfg, err := added.apply(file, applyInput{tokens: func(string) (string, bool) { return "", false }})
	if err != nil || cfg.Aliases["default"] != "n" || len(cfg.Models) != 2 {
		t.Fatal("apply", err)
	}
	if got := changed[Routing, *Routing](&doc, &added); !slices.Equal(got, []string{"aliases.default", "models.n"}) {
		t.Fatal("changed", got)
	}
	// Removing a model still used by an alias or a group policy is refused.
	removed := Routing{Models: map[string]config.Model{"n": doc.Models["m"]}, Aliases: map[string]string{"default": "m"}}
	_, err = removed.apply(file, applyInput{tokens: func(string) (string, bool) { return "", false }})
	if err == nil || !strings.Contains(err.Error(), `aliases.default: "m" is not a configured model`) || !strings.Contains(err.Error(), `groups.staff.models: "m" is not a configured model`) {
		t.Fatal("dependency check", err)
	}
	if conflicts := removed.conflicts(file, nil, true); len(conflicts) != 1 || conflicts[0].Setting != "models.m" || removed.conflictsBlock() {
		t.Fatal("routing conflicts are informational", conflicts)
	}
	empty := Routing{Models: map[string]config.Model{}}
	if len(empty.problems()) != 1 {
		t.Fatal("a routing document needs a model")
	}
}
