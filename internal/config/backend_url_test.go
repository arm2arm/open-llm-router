package config

import (
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestManagedBackendURLsAcceptHostsAndAPIPaths(t *testing.T) {
	base, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	for raw, want := range map[string]string{
		"http://141.33.4.201:8081/v1":        "http://141.33.4.201:8081",
		"http://141.33.4.201:8081/v1/models": "http://141.33.4.201:8081",
		" https://GPU.EXAMPLE:443/v1/ ":      "https://gpu.example",
		"https://gpu.example/v1/messages":    "https://gpu.example",
		"https://gpu.example/v1/responses":   "https://gpu.example",
		"gpu.cluster:8000/v1":                "http://gpu.cluster:8000",
		"http://[fd00::1]:80/api/tags":       "http://[fd00::1]",
	} {
		t.Run(raw, func(t *testing.T) {
			models := maps.Clone(base.Models)
			model := models["chat-model"]
			model.Pool = append([]Replica(nil), model.Pool...)
			model.Pool[0].URL = raw
			models["chat-model"] = model
			next, err := WithModels(base, models, func(string, string) (string, bool) { return "", false })
			if err != nil || next.Models["chat-model"].Pool[0].URL != want {
				t.Fatalf("expected %s: %v", want, err)
			}
			if models["chat-model"].Pool[0].URL != raw {
				t.Fatal("normalization mutated input")
			}
		})
	}
}

func TestBackendURLNormalizationRejectsUnsafeOrUnsupportedInput(t *testing.T) {
	for _, raw := range []string{
		"", "file:///etc/passwd", "https://user:secret@host/v1",
		"https://host/v1?key=secret", "https://host/v1#secret",
		"https://host/v1?", "https://host/v1#", "https://host/prefix/v1",
		"https://host/%76%31", "https://host:bad/v1",
	} {
		if _, err := NormalizeBackendURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe input accepted or echoed: %q", raw)
		}
	}
}

func TestEquivalentReplicaOriginsAreRejectedAcrossFileAndManagedRouting(t *testing.T) {
	base, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"http://a:8000", "http://a:8000/"}, {"http://a:8000", "HTTP://A:8000"}, {"http://a", "http://A:80/"}, {"https://a:443", "HTTPS://A/"}} {
		input := fmt.Sprintf("models:\n  m:\n    protocols: [openai]\n    timeout_s: 5\n    pool: [{url: %q, weight: 1, max_inflight: 4}, {url: %q, weight: 1, max_inflight: 4}]\n", pair[0], pair[1])
		if _, err := Parse([]byte(input), func(string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "listed twice") {
			t.Fatalf("file permits equivalent replicas %v: %v", pair, err)
		}
		models := maps.Clone(base.Models)
		m := models["chat-model"]
		m.Pool = []Replica{{URL: pair[0], Weight: 1, MaxInflight: 4}, {URL: pair[1], Weight: 1, MaxInflight: 4}}
		models["chat-model"] = m
		if _, err := WithPolicies(base, models, base.Aliases, base.Groups, func(string, string) (string, bool) { return "", false }); err == nil || !strings.Contains(err.Error(), "listed twice") {
			t.Fatalf("managed routing permits equivalent replicas %v: %v", pair, err)
		}
	}
	single := []byte("models:\n  m:\n    protocols: [openai]\n    timeout_s: 5\n    pool: [{url: 'HTTP://A:8000/', weight: 1, max_inflight: 4}]\n")
	cfg, err := Parse(single, func(string) (string, bool) { return "", false })
	if err != nil || cfg.Models["m"].Pool[0].URL != "http://a:8000" {
		t.Fatal("file origin not normalized", err)
	}
}
