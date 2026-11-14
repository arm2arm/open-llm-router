package settings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestBackendOriginAndCredentialBinding(t *testing.T) {
	if origin, err := NormalizeBackendURL("https://GPU.EXAMPLE:443/v1"); err != nil || origin != "https://gpu.example" {
		t.Fatal("default port not canonicalized", origin, err)
	}
	for _, raw := range []string{"HTTP://GPU.EXAMPLE:8000/v1/", "http://gpu.example:8000/v1/models", "http://gpu.example:8000", "gpu.example:8000", "gpu.example:8000/v1"} {
		origin, err := NormalizeBackendURL(raw)
		if err != nil || origin != "http://gpu.example:8000" {
			t.Fatalf("normalize %s: %s %v", raw, origin, err)
		}
	}
	for _, raw := range []string{"http://user:secret@host", "http://host?key=secret", "http://host/arbitrary/path", "file:///etc/passwd"} {
		if _, err := NormalizeBackendURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe origin accepted or echoed")
		}
	}
	backend := &Backend{URL: "http://gpu.example:8000", HealthPath: "/v1/models", PrivateNetwork: true}
	original := backend.SecretName("gpu")
	backend.URL = "http://other.example:8000"
	if original == backend.SecretName("gpu") || original == backend.SecretName("other") {
		t.Fatal("API key binding did not include identity and origin")
	}
	backend.APIKey.Env = "DATABASE_PASSWORD"
	if len((&Routing{Backends: map[string]*Backend{"gpu": backend}}).backendProblems()) == 0 {
		t.Fatal("arbitrary environment credential accepted")
	}
}

func TestRoutingDecodeCanonicalizesBackendAndManualReplicaURLs(t *testing.T) {
	var doc Routing
	if err := doc.decode([]byte(`{"models":{"manual":{"pool":[{"url":"http://141.33.4.201:8081/v1"}]},"managed":{"pool":[{"backend":"gpu","url":""}]}},"backends":{"gpu":{"url":"https://GPU.EXAMPLE:443/v1/models"}}}`)); err != nil {
		t.Fatal(err)
	}
	if doc.Models["manual"].Pool[0].URL != "http://141.33.4.201:8081" || doc.Backends["gpu"].URL != "https://gpu.example" || doc.Models["managed"].Pool[0].URL != "" {
		t.Fatal("routing document did not retain canonical connection URLs")
	}
}

func TestBackendGroupsAndPresetValidation(t *testing.T) {
	file := parse(t, "", nil)
	var doc Routing
	doc.baseline(file)
	doc.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
	doc.Backends = map[string]*Backend{"gpu": {Name: "Research cluster", URL: "http://10.1.2.3:8000", HealthPath: "/health", Groups: []string{"staff"}, Preset: "small"}}
	if problems := doc.problems(); len(problems) > 0 {
		t.Fatal(problems)
	}
	cfg, err := doc.apply(file, applyInput{})
	if err != nil || cfg.Backends["gpu"].Rate.RPM == nil || *cfg.Backends["gpu"].Rate.RPM != 20 || len(cfg.Backends["gpu"].Groups) != 1 {
		t.Fatal("backend policy not applied", err)
	}
	doc.Backends["gpu"].Groups = []string{"unknown"}
	if len(doc.problems()) == 0 {
		t.Fatal("unknown group accepted")
	}
	doc.Backends["gpu"].Groups = []string{}
	doc.Backends["gpu"].Preset = "missing"
	if len(doc.problems()) == 0 {
		t.Fatal("missing preset accepted")
	}
	doc.Backends["gpu"].Preset = "unlimited"
	doc.BackendPresets["unlimited"] = config.BackendRate{Name: "Unlimited", RPM: new(int64(1))}
	if len(doc.problems()) == 0 {
		t.Fatal("limited unlimited preset accepted")
	}
}

func TestManagedBackendResolvesRoutesWithoutPersistingKey(t *testing.T) {
	file := parse(t, "", nil)
	var doc Routing
	doc.baseline(file)
	backend := &Backend{URL: "http://gpu.example:8000", HealthPath: "/v1/models", PrivateNetwork: true}
	doc.Backends = map[string]*Backend{"gpu": backend}
	model := doc.Models["m"]
	model.Pool = []config.Replica{{Backend: "gpu", Weight: 1, MaxInflight: 8}}
	doc.Models["m"] = model
	cfg, err := doc.apply(file, applyInput{secrets: map[string]string{backend.SecretName("gpu"): "synthetic-backend-key"}})
	if err != nil {
		t.Fatal(err)
	}
	replica := cfg.Models["m"].Pool[0]
	if replica.URL != backend.URL || replica.HealthPath != backend.HealthPath || !replica.PrivateNetwork || replica.BearerToken != "synthetic-backend-key" {
		t.Fatal("backend not resolved")
	}
	if model.Pool[0].URL != "" {
		t.Fatal("document mutated by runtime resolution")
	}
	encoded, _ := json.Marshal(cfg.Models)
	if strings.Contains(string(encoded), "synthetic-backend-key") {
		t.Fatal("runtime secret serialized")
	}
	delete(doc.Backends, "gpu")
	if _, err := doc.apply(file, applyInput{}); err == nil {
		t.Fatal("missing backend accepted")
	}
}

func TestBackendCanPrecedeAllModels(t *testing.T) {
	file, err := config.Parse([]byte("models: {}\ngroups: {admins: {models: ['*'], admin: true}}\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	var doc Routing
	doc.baseline(file)
	doc.Backends = map[string]*Backend{"gpu": {URL: "http://10.1.2.3:8000", PrivateNetwork: true, HealthPath: "/v1/models"}}
	if problems := doc.problems(); len(problems) != 0 {
		t.Fatal(problems)
	}
	if cfg, err := doc.apply(file, applyInput{}); err != nil || len(cfg.Models) != 0 {
		t.Fatal("backend-only routing refused", err)
	}
}
