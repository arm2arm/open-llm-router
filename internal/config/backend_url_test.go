package config

import (
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
		"gpu.cluster:8000/v1":                "http://gpu.cluster:8000",
		"http://[fd00::1]:80/api/tags":       "http://[fd00::1]",
	} {
		t.Run(raw, func(t *testing.T) {
			models := maps.Clone(base.Models)
			model := models["chat-model"]
			model.Pool = append([]Replica(nil), model.Pool...)
			model.Pool[0].URL = raw
			models["chat-model"] = model
			next, err := WithModels(base, models, lookupIn(nil))
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
