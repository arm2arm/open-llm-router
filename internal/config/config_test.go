package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// valid is a correct config. The tests below change one thing in it at a time.
// Its line numbers matter: the tests check the line named in an error.
const valid = `oidc:
  issuer: ${OIDC_ISSUER}
  portal_client_id: olr-portal
models:
  chat-model:
    protocols: [openai, anthropic]
    timeout_s: 900
    pool:
      - {url: http://backend-a:8000, weight: 2.0, max_inflight: 64}
      - {url: http://backend-b:8000, weight: 1.0, max_inflight: 32}
groups:
  staff: {models: ["*"], tokens_per_day: 2000000, rpm: 60, max_concurrent: 4}
  students: {models: [chat-model], rpm: 20}
aliases: {claude-sonnet-4-5: chat-model}
`

var validEnv = map[string]string{"OIDC_ISSUER": "http://keycloak:8080/realms/olr-dev"}

func TestEmptyCatalogSupportsInitialSetup(t *testing.T) {
	cfg, err := Parse([]byte("models: {}\ngroups: {admins: {models: ['*'], admin: true}}\n"), lookupIn(nil))
	if err != nil || len(cfg.Models) != 0 || !cfg.Groups["admins"].Admin {
		t.Fatal("empty initial catalog refused", err)
	}
	if _, err := Parse([]byte("models: {}\ngroups: {staff: {models: [missing]}}\n"), lookupIn(nil)); err == nil {
		t.Fatal("unknown group model reference accepted")
	}
}

func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

// change returns valid with the first occurrence of old replaced by new.
func change(t *testing.T, old, new string) string {
	t.Helper()
	if !strings.Contains(valid, old) {
		t.Fatalf("test setup: %q is not in the valid config", old)
	}
	return strings.Replace(valid, old, new, 1)
}

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.OIDC.Issuer; got != validEnv["OIDC_ISSUER"] {
		t.Errorf("oidc.issuer = %q, want the value of OIDC_ISSUER", got)
	}
	if cfg.OIDC.GroupsClaim != "groups" || cfg.OIDC.UsernameClaim != "preferred_username" {
		t.Errorf("claim defaults = %q, %q", cfg.OIDC.GroupsClaim, cfg.OIDC.UsernameClaim)
	}
	m := cfg.Models["chat-model"]
	if len(m.Pool) != 2 || m.Pool[0].URL != "http://backend-a:8000" || m.Pool[0].Weight != 2 || m.Pool[1].MaxInflight != 32 {
		t.Errorf("chat-model pool = %+v", m.Pool)
	}
	if m.TimeoutS != 900 || len(m.Protocols) != 2 {
		t.Errorf("chat-model = %+v", m)
	}
	staff, students := cfg.Groups["staff"], cfg.Groups["students"]
	if staff.TokensPerDay == nil || *staff.TokensPerDay != 2_000_000 || *staff.MaxConcurrent != 4 {
		t.Errorf("staff = %+v", staff)
	}
	if students.TokensPerDay != nil || students.MaxConcurrent != nil || *students.RPM != 20 {
		t.Errorf("students: limits that are left out must stay nil, got %+v", students)
	}
	if cfg.Aliases["claude-sonnet-4-5"] != "chat-model" {
		t.Errorf("aliases = %v", cfg.Aliases)
	}
}

func TestParseWithoutOIDC(t *testing.T) {
	noOIDC := valid[strings.Index(valid, "models:"):]
	cfg, err := Parse([]byte(noOIDC), lookupIn(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC != nil {
		t.Errorf("OIDC = %+v, want nil when the section is left out", cfg.OIDC)
	}
}

func TestPortalEnvironment(t *testing.T) {
	data := valid[strings.Index(valid, "models:"):]
	env := map[string]string{"OIDC_ISSUER": "https://login.example/realm", "OIDC_PORTAL_CLIENT_ID": "another-client", "OIDC_PORTAL_CLIENT_SECRET": "test-secret", "OIDC_GROUPS_CLAIM": "memberships", "OIDC_USERNAME_CLAIM": "name", "OIDC_BACKCHANNEL_URL": "http://internal/realm", "PORTAL_BASE_URL": "https://portal.example/", "PORTAL_COOKIE_SECURE": "true"}
	cfg, err := Parse([]byte(data), lookupIn(env))
	if err != nil || cfg.OIDC.PortalClientID != "another-client" || cfg.OIDC.GroupsClaim != "memberships" || cfg.OIDC.UsernameClaim != "name" || cfg.OIDCClientSecret != "test-secret" || cfg.Portal.BaseURL != "https://portal.example" || !cfg.Portal.CookieSecure {
		t.Fatal("environment-only OIDC configuration failed", err)
	}
	for _, tc := range []struct{ name, value string }{{"PORTAL_BASE_URL", "https://portal.example/path"}, {"PORTAL_COOKIE_SECURE", "maybe"}, {"OIDC_BACKCHANNEL_URL", "https://name:secret@internal/realm"}, {"OIDC_ISSUER", "https://login.example/realm?query=1"}} {
		copy := make(map[string]string)
		for k, v := range env {
			copy[k] = v
		}
		copy[tc.name] = tc.value
		if _, err := Parse([]byte(data), lookupIn(copy)); err == nil {
			t.Fatal("invalid setting accepted", tc.name)
		}
	}
	cfg, err = Parse([]byte(data), lookupIn(nil))
	if err != nil || !cfg.Portal.CookieSecure {
		t.Fatal("cookies must be secure by default", err)
	}
}

func TestIssuerErrorDoesNotExposeCredentials(t *testing.T) {
	_, err := Parse([]byte(valid), lookupIn(map[string]string{"OIDC_ISSUER": "https://name:private-password@login.example/realm"}))
	if err == nil || !strings.Contains(err.Error(), "oidc.issuer:") {
		t.Fatal("credential-bearing issuer must be rejected")
	}
	if strings.Contains(err.Error(), "private-password") {
		t.Fatal("issuer validation exposed credentials")
	}
}

func TestLimitsTimezone(t *testing.T) {
	for _, zone := range []string{"", "UTC", "Europe/Berlin", "Mars/Unknown", "Local"} {
		data := valid
		if zone != "" {
			data += "limits: {timezone: " + zone + "}\n"
		}
		cfg, err := Parse([]byte(data), lookupIn(validEnv))
		if zone == "Mars/Unknown" || zone == "Local" {
			if err == nil || !strings.Contains(err.Error(), "limits.timezone") {
				t.Fatal(zone, err)
			}
		} else {
			want := zone
			if want == "" {
				want = "UTC"
			}
			if err != nil || cfg.Limits.Timezone != want || cfg.Limits.Location.String() != want {
				t.Fatal(zone, cfg, err)
			}
		}
	}
	if _, err := Parse([]byte(valid+"limits: {timezone: ''}\n"), lookupIn(validEnv)); err == nil {
		t.Fatal("empty timezone accepted")
	}
}

func TestKeyLifetime(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want int
	}{
		{"", 90},
		{"auth: {default_key_lifetime_days: 7}\n", 7},
		{"auth: {default_key_lifetime_days: 0}\n", 0},
		{"auth: {default_key_lifetime_days: -1}\n", 0},
		{"auth: {default_key_lifetime_days: 36501}\n", 0},
	} {
		cfg, err := Parse([]byte(tc.yaml+valid), lookupIn(validEnv))
		if tc.want == 0 {
			if err == nil || !strings.Contains(err.Error(), "auth.default_key_lifetime_days") {
				t.Fatalf("invalid lifetime accepted: %v", err)
			}
		} else if err != nil || cfg.Auth.DefaultKeyLifetimeDays != tc.want {
			t.Fatalf("key lifetime: cfg=%+v err=%v", cfg, err)
		}
	}
}

func TestHealthPath(t *testing.T) {
	for _, path := range []string{"/", "/health", "/health/ready", "", "health", "https://other/health", "//other/health", "/health?x=1", "/health#fragment", "/%zz"} {
		t.Run(path, func(t *testing.T) {
			data := change(t, "url: http://backend-a:8000", "url: http://backend-a:8000, health_path: '"+path+"'")
			_, err := Parse([]byte(data), lookupIn(validEnv))
			valid := path == "" || path == "/" || path == "/health" || path == "/health/ready"
			if (err == nil) != valid {
				t.Fatalf("path %q: %v", path, err)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want []string // each must be part of the error
	}{
		// Strict reading of the file.
		{name: "unknown top-level key",
			yaml: valid + "priority_classes: {batch: 10}\n",
			want: []string{"line 15:", "priority_classes"}},
		{name: "misspelled key",
			yaml: change(t, "max_inflight: 32", "max_inflght: 32"),
			want: []string{"line 10:", "max_inflght"}},
		{name: "secret in the file",
			yaml: valid + "database_url: postgres://olr:hunter2@db/olr\n",
			want: []string{"line 15:", "database_url"}},
		{name: "wrong type",
			yaml: change(t, "timeout_s: 900", "timeout_s: soon"),
			want: []string{"line 7:", "soon"}},
		{name: "key given twice",
			yaml: change(t, "    timeout_s: 900\n", "    timeout_s: 900\n    timeout_s: 60\n"),
			want: []string{"line 8:", "timeout_s"}},
		{name: "empty file",
			yaml: "\n",
			want: []string{"empty"}},

		// ${VAR} references.
		{name: "variable not set",
			yaml: valid, env: map[string]string{},
			want: []string{"line 2: environment variable OIDC_ISSUER is not set"}},
		{name: "all unset variables are reported at once",
			yaml: change(t, "http://backend-a:8000", `"${BACKEND_A}"`), env: map[string]string{},
			want: []string{"line 2: environment variable OIDC_ISSUER is not set", "line 9: environment variable BACKEND_A is not set"}},
		{name: "shell default syntax",
			yaml: change(t, "${OIDC_ISSUER}", "${OIDC_ISSUER:-http://localhost}"),
			want: []string{"line 2:", "only the form ${NAME} is supported"}},
		{name: "reference without closing brace",
			yaml: change(t, "${OIDC_ISSUER}", "${OIDC_ISSUER"),
			want: []string{"line 2:", "without a closing"}},
		{name: "unquoted reference inside braces",
			yaml: change(t, "{url: http://backend-a:8000,", "{url: ${BACKEND_A},"),
			env:  map[string]string{"OIDC_ISSUER": "http://kc", "BACKEND_A": "http://a:8000"},
			want: []string{"line 9:", `must be quoted: "${NAME}"`}},

		// Validation.
		{name: "issuer is not a URL",
			yaml: valid, env: map[string]string{"OIDC_ISSUER": "keycloak:8080"},
			want: []string{"oidc.issuer:", "http or https URL"}},
		{name: "issuer is empty",
			yaml: valid, env: map[string]string{"OIDC_ISSUER": ""},
			want: []string{"oidc.issuer:"}},
		{name: "no client id",
			yaml: change(t, "  portal_client_id: olr-portal\n", ""),
			want: []string{"oidc.portal_client_id: must be set"}},
		{name: "unknown protocol",
			yaml: change(t, "[openai, anthropic]", "[openai, gemini]"),
			want: []string{"models.chat-model.protocols:", `"gemini"`}},
		{name: "no protocols",
			yaml: change(t, "[openai, anthropic]", "[]"),
			want: []string{"models.chat-model.protocols:"}},
		{name: "timeout left out",
			yaml: change(t, "    timeout_s: 900\n", ""),
			want: []string{"models.chat-model.timeout_s: must be greater than 0"}},
		{name: "empty pool",
			yaml: "models:\n  m: {protocols: [openai], timeout_s: 60, pool: []}\n",
			want: []string{"models.m.pool: at least one replica is required"}},
		{name: "replica URL without scheme",
			yaml: change(t, "http://backend-b:8000", "backend-b:8000"),
			want: []string{"models.chat-model.pool[1].url:", `"backend-b:8000"`}},
		{name: "replica listed twice",
			yaml: change(t, "http://backend-b:8000", "http://backend-a:8000"),
			want: []string{"models.chat-model.pool[1].url:", "listed twice"}},
		{name: "weight left out",
			yaml: change(t, "weight: 1.0, ", ""),
			want: []string{"models.chat-model.pool[1].weight: must be greater than 0"}},
		{name: "max_inflight of zero",
			yaml: change(t, "max_inflight: 32", "max_inflight: 0"),
			want: []string{"models.chat-model.pool[1].max_inflight: must be greater than 0"}},
		{name: "alias to a model that does not exist",
			yaml: change(t, "claude-sonnet-4-5: chat-model", "claude-sonnet-4-5: chat-modle"),
			want: []string{"aliases.claude-sonnet-4-5:", `"chat-modle" is not a configured model`}},
		{name: "alias with the name of a model",
			yaml: change(t, "claude-sonnet-4-5: chat-model", "chat-model: chat-model"),
			want: []string{`aliases: "chat-model" cannot be used as an alias`}},
		{name: "group with a model that does not exist",
			yaml: change(t, "models: [chat-model]", "models: [chat-modle]"),
			want: []string{"groups.students.models:", `"chat-modle" is not a configured model`}},
		{name: "group without models",
			yaml: change(t, "{models: [chat-model], rpm: 20}", "{rpm: 20}"),
			want: []string{"groups.students.models:"}},
		{name: "limit of zero",
			yaml: change(t, "rpm: 20", "rpm: 0"),
			want: []string{"groups.students.rpm: must be greater than 0; leave it out for no limit"}},
		{name: "all validation problems are reported at once",
			yaml: change(t, "weight: 1.0, max_inflight: 32", "weight: -1, max_inflight: 0"),
			want: []string{"pool[1].weight:", "pool[1].max_inflight:"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.env
			if env == nil {
				env = validEnv
			}
			cfg, err := Parse([]byte(tt.yaml), lookupIn(env))
			if err == nil {
				t.Fatalf("no error; got config %+v", cfg)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q:%v", want, err)
				}
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error repeats a secret from the file:%v", err)
			}
		})
	}
}

func TestExpansion(t *testing.T) {
	t.Run("a reference in a comment is ignored", func(t *testing.T) {
		yaml := "# backend_token: ${NOT_SET_ANYWHERE}\n" + change(t, "timeout_s: 900", "timeout_s: 900 # was ${OLD_TIMEOUT}")
		if _, err := Parse([]byte(yaml), lookupIn(validEnv)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unquoted references can give numbers", func(t *testing.T) {
		yaml := change(t, "    timeout_s: 900\n", "    timeout_s: ${TIMEOUT}\n")
		yaml = strings.Replace(yaml, "- {url: http://backend-b:8000, weight: 1.0, max_inflight: 32}",
			"- url: http://${HOST_B}:8000\n        weight: ${WEIGHT_B}\n        max_inflight: ${INFLIGHT_B}", 1)
		cfg, err := Parse([]byte(yaml), lookupIn(map[string]string{
			"OIDC_ISSUER": "http://kc", "TIMEOUT": "120", "HOST_B": "gpu-b", "WEIGHT_B": "0.5", "INFLIGHT_B": "16",
		}))
		if err != nil {
			t.Fatal(err)
		}
		m := cfg.Models["chat-model"]
		if m.TimeoutS != 120 || m.Pool[1] != (Replica{URL: "http://gpu-b:8000", Weight: 0.5, MaxInflight: 16}) {
			t.Errorf("chat-model = %+v", m)
		}
	})

	t.Run("keys and quoted references inside braces", func(t *testing.T) {
		yaml := strings.NewReplacer(
			"  chat-model:\n", "  ${MODEL}:\n",
			"models: [chat-model]", `models: ["${MODEL}"]`,
			"claude-sonnet-4-5: chat-model", `claude-sonnet-4-5: "${MODEL}"`,
			"{url: http://backend-a:8000,", `{url: "${BACKEND_A}",`,
		).Replace(valid)
		cfg, err := Parse([]byte(yaml), lookupIn(map[string]string{
			"OIDC_ISSUER": "http://kc", "MODEL": "org/private-model-27b", "BACKEND_A": "http://gpu-a:8000",
		}))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := cfg.Models["org/private-model-27b"]
		if !ok || m.Pool[0].URL != "http://gpu-a:8000" {
			t.Errorf("models = %+v", cfg.Models)
		}
		if cfg.Aliases["claude-sonnet-4-5"] != "org/private-model-27b" {
			t.Errorf("aliases = %v", cfg.Aliases)
		}
	})

	// The value of a variable is data. Whatever it contains, it must arrive as
	// that one string and never be read as YAML.
	for _, value := range []string{
		"a: b # c",
		"x\nmodels: {}\n",
		`{"quoted": [1, 2]}`,
		"*anchor",
		"007",
		"1e3",
		"0x1F",
		"true",
		" padded ",
	} {
		t.Run("value "+value, func(t *testing.T) {
			yaml := change(t, "portal_client_id: olr-portal", "portal_client_id: ${CLIENT_ID}")
			cfg, err := Parse([]byte(yaml), lookupIn(map[string]string{"OIDC_ISSUER": "http://kc", "CLIENT_ID": value}))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.OIDC.PortalClientID != value {
				t.Errorf("portal_client_id = %q, want %q", cfg.OIDC.PortalClientID, value)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Run("the private pilot template is valid", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", "pilot.config.example.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{
			"OIDC_ISSUER":           "https://login.example.org/realms/pilot",
			"OIDC_PORTAL_CLIENT_ID": "olr-portal", "OIDC_AUDIENCE": "olr-api", "OIDC_SYNC_CLIENT_ID": "olr-sync",
			"PORTAL_BASE_URL": "https://router.example.org", "OIDC_PORTAL_CLIENT_SECRET": "test-portal",
			"OLR_TIMEZONE": "Europe/Berlin", "OLR_USAGE_DAYS": "90", "OLR_USAGE_DAILY_DAYS": "400",
			"OLR_UPSTREAM_MODEL": "served-model", "OLR_BACKEND_A_URL": "https://gpu-a.example.org",
			"OLR_BACKEND_B_URL": "https://gpu-b.example.org", "OLR_BACKEND_A_WEIGHT": "2",
			"OLR_BACKEND_B_WEIGHT": "1", "OLR_BACKEND_A_MAX_INFLIGHT": "4", "OLR_BACKEND_B_MAX_INFLIGHT": "2",
			"VLLM_A_API_KEY": "test-a", "VLLM_B_API_KEY": "test-b", "OLR_PILOT_TOKENS_PER_DAY": "500000",
			"OLR_PILOT_TOKENS_PER_MONTH": "5000000", "OLR_PILOT_RPM": "20", "OLR_PILOT_TPM": "20000",
			"OLR_PILOT_MAX_CONCURRENT": "2",
		}
		cfg, err := Parse(data, lookupIn(env))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Portal.CookieSecure || cfg.OIDC.BackchannelURL != "" {
			t.Fatal("pilot must use secure cookies and public discovery")
		}
		model := cfg.Models["chat-model"]
		if model.UpstreamModel != "served-model" || model.Pool[0].BearerToken != "test-a" || model.Pool[1].MaxInflight != 2 {
			t.Fatal("site model, credentials or capacity did not reach the pool")
		}
	})

	t.Run("the example config is valid", func(t *testing.T) {
		t.Setenv("OIDC_ISSUER", "http://keycloak:8080/realms/olr-dev")
		t.Setenv(EnvDatabaseURL, "postgres://olr:hunter2@postgres/olr")
		t.Setenv(EnvOIDCClientSecret, "s3cret")
		cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Models["chat-model"].Pool) != 2 || len(cfg.Groups) != 3 {
			t.Errorf("unexpected example content: %+v", cfg)
		}
		if cfg.DatabaseURL != "postgres://olr:hunter2@postgres/olr" || cfg.OIDCClientSecret != "s3cret" {
			t.Errorf("secrets were not read from the environment")
		}
	})

	t.Run("an invalid file is named with every problem on its own line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "olr.yaml")
		bad := change(t, "max_inflight: 32", "max_inflght: 32") + "priority: 10\n"
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OIDC_ISSUER", "http://kc")
		_, err := Load(path)
		var problems Problems
		if !errors.As(err, &problems) || len(problems) != 2 {
			t.Fatalf("want two problems, got: %v", err)
		}
		if !strings.HasPrefix(err.Error(), "config file "+path+":\n  line 10: ") || !strings.Contains(err.Error(), "\n  line 15: ") {
			t.Errorf("unexpected message layout:\n%v", err)
		}
	})

	t.Run("a missing file is named", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestRoutingSettings(t *testing.T) {
	yaml := change(t, "timeout_s: 900", "timeout_s: 900\n    idle_timeout_s: 45\n    upstream_model: served-name")
	yaml = strings.Replace(yaml, "weight: 2.0,", "bearer_token_env: BACKEND_KEY, weight: 2.0,", 1)
	env := map[string]string{"OIDC_ISSUER": "http://kc", "BACKEND_KEY": "private-backend-key"}
	cfg, err := Parse([]byte(yaml), lookupIn(env))
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.Models["chat-model"]
	if model.IdleTimeoutS != 45 || model.UpstreamModel != "served-name" || model.Pool[0].BearerToken != env["BACKEND_KEY"] {
		t.Fatal("routing settings not loaded")
	}
	for _, token := range []string{"", "bad\r\nheader"} {
		env["BACKEND_KEY"] = token
		if _, err := Parse([]byte(yaml), lookupIn(env)); err == nil || !strings.Contains(err.Error(), "bearer_token_env") || strings.Contains(err.Error(), "bad\r") {
			t.Fatal("invalid token not safely rejected")
		}
	}
	delete(env, "BACKEND_KEY")
	if _, err := Parse([]byte(yaml), lookupIn(env)); err == nil {
		t.Fatal("missing backend token accepted")
	}
	for _, tc := range []struct{ old, new string }{
		{"weight: 1.0", "weight: .nan"},
		{"weight: 1.0", "weight: .inf"},
		{"timeout_s: 900", "timeout_s: 900\n    idle_timeout_s: -1"},
		{"http://backend-b:8000", "http://backend-b:8000/path"},
		{"http://backend-b:8000", "http://backend-b:8000?query=value"},
	} {
		if _, err := Parse([]byte(change(t, tc.old, tc.new)), lookupIn(validEnv)); err == nil {
			t.Fatalf("accepted %s", tc.new)
		}
	}
}

func TestForwarders(t *testing.T) {
	cfg, err := Parse([]byte(valid+"forwarders:\n  header_map: {x-openwebui-user-email: email}\n  unknown_user: forwarder_policy\n"), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Forwarders.HeaderMap["X-Openwebui-User-Email"] != "email" || cfg.Forwarders.HeaderMap["X-Olr-End-User"] != "username" || cfg.Forwarders.UnknownUser != "forwarder_policy" {
		t.Fatalf("%+v", cfg.Forwarders)
	}
	for _, bad := range []string{"forwarders: {unknown_user: allow}\n", "forwarders: {header_map: {X-User: phone}}\n"} {
		if _, err := Parse([]byte(valid+bad), lookupIn(validEnv)); err == nil || !strings.Contains(err.Error(), "forwarders.") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestBodyLimits(t *testing.T) {
	cfg, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxBodyBytes != 4<<20 || cfg.Limits.MaxBodyReaders != 32 || cfg.Limits.MaxBodyReadersPerUser != 4 {
		t.Fatalf("upload defaults: %+v", cfg.Limits)
	}
	for _, field := range []string{"max_body_bytes", "max_body_readers", "max_body_readers_per_user"} {
		for _, value := range []string{"0", "-1"} {
			_, err := Parse([]byte(valid+"limits: {"+field+": "+value+"}\n"), lookupIn(validEnv))
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s=%s accepted: %v", field, value, err)
			}
		}
	}
	cfg, err = Parse([]byte(valid+"limits: {max_body_bytes: 1024, max_body_readers: 2, max_body_readers_per_user: 1}\n"), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxBodyBytes != 1024 || cfg.Limits.MaxBodyReaders != 2 || cfg.Limits.MaxBodyReadersPerUser != 1 {
		t.Fatalf("upload overrides: %+v", cfg.Limits)
	}
}

func TestKeyPolicyValidation(t *testing.T) {
	for _, input := range []string{
		"auth: {max_key_lifetime_days: 0}",
		"auth: {max_key_lifetime_days: 30, default_key_lifetime_days: 31}",
		"auth: {max_active_keys_per_user: 0}",
		"auth: {max_active_keys_per_user: 1001}",
	} {
		if _, err := Parse([]byte(input+"\n"+valid), lookupIn(validEnv)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}

func TestContentTracingPolicy(t *testing.T) {
	cfg, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil || cfg.ContentTracing.Enabled {
		t.Fatal("content capture must default off", err)
	}
	if _, err := Parse([]byte("content_tracing: {enabled: true, redact_patterns: ['[']}\n"+valid), lookupIn(validEnv)); err == nil {
		t.Fatal("invalid redaction accepted")
	}
}

func TestProductionDatabaseTLS(t *testing.T) {
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		err := ValidateDatabaseTLS("postgres://user:secret@db.example.org/router?sslmode=" + mode)
		if (err == nil) != (mode == "verify-full") {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if err := ValidateDatabaseTLS("private-secret-invalid-dsn"); err == nil || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("unsafe error", err)
	}
}

func TestProductionTransportPolicy(t *testing.T) {
	env := map[string]string{"OIDC_ISSUER": "https://login.example.org/realm", "PORTAL_BASE_URL": "https://router.example.org"}
	secure := strings.ReplaceAll(valid, "http://backend-", "https://backend-") + "security: {require_tls: true}\n"
	cfg, err := Parse([]byte(secure), lookupIn(env))
	if err != nil || !cfg.OIDC.RequireTLS {
		t.Fatal("secure config rejected", err)
	}
	for _, bad := range []string{strings.Replace(secure, "https://backend-a", "http://backend-a", 1)} {
		if _, err := Parse([]byte(bad), lookupIn(env)); err == nil {
			t.Fatal("plaintext config accepted")
		}
	}
	env["PORTAL_BASE_URL"] = "http://router.example.org"
	if _, err := Parse([]byte(secure), lookupIn(env)); err == nil {
		t.Fatal("plaintext portal accepted")
	}
}

func TestProductionTelemetryOverridesRequireTLS(t *testing.T) {
	t.Setenv("OIDC_ISSUER", "https://login.example.org/realm")
	t.Setenv("PORTAL_BASE_URL", "https://router.example.org")
	t.Setenv("PORTAL_COOKIE_SECURE", "true")
	t.Setenv(EnvDatabaseURL, "postgres://user:secret@db.example.org/router?sslmode=verify-full")
	path := filepath.Join(t.TempDir(), "config.yaml")
	secure := strings.ReplaceAll(valid, "http://backend-", "https://backend-") + "security: {require_tls: true}\n"
	if err := os.WriteFile(path, []byte(secure), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.org")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://collector.example.org/v1/traces")
			if _, err := Load(path); err != nil {
				t.Fatal(err)
			}
			t.Setenv(name, "http://collector.example.org")
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "HTTPS telemetry") {
				t.Fatal("plaintext telemetry override accepted", err)
			}
		})
	}
}
