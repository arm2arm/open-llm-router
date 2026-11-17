package backendpolicy

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestManagedDestinationAndCredentialPolicy(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: "http://127.0.0.1:8000", BearerTokenEnv: "BACKEND_TOKEN", BearerToken: "backend-secret"}}}}}
	env := map[string]string{"OLR_BACKEND_ALLOWED_CIDRS": "192.168.178.0/24,fc00::/7", "OLR_BACKEND_TOKEN_ENVS": "SECOND_TOKEN", "OLR_BACKEND_TOKEN_ORIGINS": `{"SECOND_TOKEN":["https://second.example"]}`, "SECOND_TOKEN": "second-secret", "DATABASE_PASSWORD": "database-secret"}
	policy, err := New(cfg, func(name string) (string, bool) { v, ok := env[name]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"http://127.0.0.1:8000", "http://192.168.178.81:11434", "https://8.8.8.8"} {
		if err := policy.Check(t.Context(), url); err != nil {
			t.Errorf("allowed destination: %s: %v", url, err)
		}
	}
	for _, url := range []string{"http://127.0.0.1:8080", "http://169.254.169.254", "http://100.100.100.200", "http://[fd00:ec2::254]", "http://192.168.2.1", "http://0.0.0.0", "http://[::1]", "http://user:private-password@192.168.178.81", "http://192.168.178.81/v1", "http://192.168.178.81?token=private-password", "ftp://192.168.178.81"} {
		err := policy.Check(t.Context(), url)
		if err == nil || strings.Contains(err.Error(), "private-password") {
			t.Errorf("unsafe destination/error: %s", url)
		}
	}
	if _, ok := policy.Token("DATABASE_PASSWORD", "http://127.0.0.1:8000"); ok {
		t.Fatal("unrelated process credential exposed")
	}
	if value, ok := policy.Token("BACKEND_TOKEN", "http://127.0.0.1:8000"); !ok || value != "backend-secret" {
		t.Fatal("file credential lost")
	}
	if value, ok := policy.Token("SECOND_TOKEN", "https://second.example"); !ok || value != "second-secret" {
		t.Fatal("allowlisted credential lost")
	}
	if len(policy.References()) != 2 {
		t.Fatal("incorrect reference inventory")
	}
	if _, err := policy.DialContext(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("metadata dial allowed")
	}
	if policy.allowed(netip.MustParseAddr("::ffff:169.254.169.254")) {
		t.Fatal("mapped metadata address allowed")
	}
	if policy.allowed(netip.MustParseAddr("100.64.1.1")) {
		t.Fatal("shared private address space allowed without an operator grant")
	}
	policy.networks = append(policy.networks, netip.MustParsePrefix("100.64.0.0/10"))
	if !policy.allowed(netip.MustParseAddr("100.64.1.1")) || policy.allowed(netip.MustParseAddr("100.100.100.200")) {
		t.Fatal("shared-network grant did not retain metadata exclusion")
	}
	policy.trusted["file-backend:8000"] = true
	if destinations, err := policy.addresses(t.Context(), "FILE-BACKEND:8000"); err != nil || len(destinations) != 1 || destinations[0] != "file-backend:8000" {
		t.Fatal("file-defined DNS trust was not case insensitive", err)
	}
	for _, value := range []string{"0.1.2.3", "192.0.0.192", "240.0.0.1"} {
		if policy.allowed(netip.MustParseAddr(value)) {
			t.Fatal("control destination allowed", value)
		}
	}
}

func TestInvalidDestinationPolicySettings(t *testing.T) {
	for key, value := range map[string]string{"OLR_BACKEND_ALLOWED_CIDRS": "not-a-cidr", "OLR_BACKEND_TOKEN_ENVS": "MISSING_TOKEN"} {
		if _, err := New(&config.Config{}, func(name string) (string, bool) { return value, name == key }); err == nil {
			t.Fatal("invalid deployment setting accepted", key)
		}
	}
}

func TestProductionPolicyRejectsPlaintextBackend(t *testing.T) {
	p, err := New(&config.Config{Security: config.Security{RequireTLS: true}}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check(t.Context(), "http://example.org"); err == nil {
		t.Fatal("plaintext inference allowed")
	}
}

func TestDialPreservesContextFailure(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: "http://127.0.0.1:1"}}}}}
	p, err := New(cfg, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.DialContext(ctx, "tcp", "127.0.0.1:1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("connection error lost its cause: %v", err)
	}
}

func TestPrivateClusterHTTPIsScopedAndRechecked(t *testing.T) {
	p, err := New(&config.Config{Security: config.Security{RequireTLS: true}}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"http://10.1.2.3:8000", "http://192.168.1.2", "http://[fd12::1]:11434", "http://141.33.4.201:8081"} {
		if err := p.CheckBackend(t.Context(), origin, true); err != nil {
			t.Fatal("private cluster refused", err)
		}
		if err := p.Check(t.Context(), origin); err == nil {
			t.Fatal("private exception leaked into normal policy")
		}
	}
	for _, origin := range []string{"http://127.0.0.1", "http://169.254.169.254", "http://[fd00:ec2::254]", "http://100.100.100.200", "http://[::1]", "http://0.0.0.0", "http://240.0.0.1"} {
		if err := p.CheckBackend(t.Context(), origin, true); err == nil {
			t.Fatal("unsafe cluster origin accepted", origin)
		}
	}
	ctx := WithPrivateNetwork(t.Context(), "http://10.1.2.3:8000")
	if _, err := p.addresses(ctx, "10.1.2.3:8000"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.addresses(ctx, "10.1.2.4:8000"); err == nil {
		t.Fatal("private grant escaped its host")
	}
}

func TestCredentialBindingsRequireExactCanonicalOrigin(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: "https://GPU.EXAMPLE:443", BearerTokenEnv: "GPU_TOKEN", BearerToken: "secret"}}}}}
	env := map[string]string{"OLR_BACKEND_TOKEN_ENVS": "EXTRA_TOKEN,UNBOUND_TOKEN", "EXTRA_TOKEN": "extra-secret", "UNBOUND_TOKEN": "unbound-secret", "OLR_BACKEND_TOKEN_ORIGINS": `{"EXTRA_TOKEN":["http://extra.example:8080"]}`}
	p, err := New(cfg, func(n string) (string, bool) { v, ok := env[n]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ref, origin string
		allowed     bool
	}{
		{"GPU_TOKEN", "https://gpu.example", true}, {"GPU_TOKEN", "https://gpu.example:443/v1", true},
		{"GPU_TOKEN", "http://gpu.example", false}, {"GPU_TOKEN", "https://gpu.example:444", false}, {"GPU_TOKEN", "https://other.example", false},
		{"EXTRA_TOKEN", "http://extra.example:8080/v1/models", true}, {"EXTRA_TOKEN", "http://extra.example", false}, {"UNBOUND_TOKEN", "https://gpu.example", false},
	} {
		value, ok := p.Token(test.ref, test.origin)
		if ok != test.allowed || !ok && value != "" {
			t.Error("unexpected credential binding", test.ref, test.origin, ok)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"EXTRA_TOKEN":[]}`, `{"MISSING":["https://extra.example"]}`, `{"EXTRA_TOKEN":["https://user:secret@extra.example"]}`} {
		env["OLR_BACKEND_TOKEN_ORIGINS"] = raw
		_, err := New(cfg, func(n string) (string, bool) { v, ok := env[n]; return v, ok })
		if err == nil || strings.Contains(err.Error(), "user:secret") {
			t.Error("invalid credential binding/error", raw)
		}
	}
}

func TestConfiguredDestinationAllowsDNSFailureButNotForbiddenAddress(t *testing.T) {
	p, err := New(&config.Config{}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	const missing = "http://olr-deliberately-missing.invalid:8000"
	if err := p.CheckBackend(t.Context(), missing, false); !errors.Is(err, ErrUnresolved) {
		t.Fatalf("live check did not report DNS failure: %v", err)
	}
	if err := p.CheckConfiguredBackend(t.Context(), missing, false); err != nil {
		t.Fatal("configuration depends on DNS availability", err)
	}
	for _, origin := range []string{"http://127.0.0.1:8000", "http://169.254.169.254", "http://192.168.2.1", "http://localhost:8000", "ftp://example.org", "http://user:secret@example.org"} {
		if err := p.CheckConfiguredBackend(t.Context(), origin, false); err == nil {
			t.Fatalf("unsafe configuration allowed: %s", origin)
		}
	}
}
