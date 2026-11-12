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
