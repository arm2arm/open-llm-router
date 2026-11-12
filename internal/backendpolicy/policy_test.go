package backendpolicy

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestManagedDestinationAndCredentialPolicy(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: "http://127.0.0.1:8000", BearerTokenEnv: "BACKEND_TOKEN", BearerToken: "backend-secret"}}}}}
	env := map[string]string{"OLR_BACKEND_ALLOWED_CIDRS": "192.168.178.0/24,fc00::/7", "OLR_BACKEND_TOKEN_ENVS": "SECOND_TOKEN", "SECOND_TOKEN": "second-secret", "DATABASE_PASSWORD": "database-secret"}
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
	if _, ok := policy.Token("DATABASE_PASSWORD"); ok {
		t.Fatal("unrelated process credential exposed")
	}
	if value, ok := policy.Token("BACKEND_TOKEN"); !ok || value != "backend-secret" {
		t.Fatal("file credential lost")
	}
	if value, ok := policy.Token("SECOND_TOKEN"); !ok || value != "second-secret" {
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
