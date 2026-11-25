package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

// The model list reports what clients set themselves up from: the context
// length detected from the backend's catalog (mock-llm reports vLLM's
// max_model_len) or configured, the model type and the protocols.
func TestModelListReportsContextLengthTypeAndProtocols(t *testing.T) {
	large := httptest.NewServer(mockllm.New(mockllm.Options{Models: []string{"chat-model", "embed-served"}, MaxModelLen: 131072}))
	defer large.Close()
	small := httptest.NewServer(mockllm.New(mockllm.Options{Models: []string{"chat-model"}, MaxModelLen: 32768}))
	defer small.Close()
	cfg := testConfig(large.URL, small.URL)
	pinned := 8192
	replica := config.Replica{URL: large.URL, Weight: 1, MaxInflight: 4}
	cfg.Models["pinned"] = config.Model{Protocols: []string{config.ProtocolOpenAI}, TimeoutS: 3, IdleTimeoutS: 2, ContextLength: &pinned, Pool: []config.Replica{replica}}
	cfg.Models["embed"] = config.Model{Protocols: []string{config.ProtocolOpenAI}, TimeoutS: 3, IdleTimeoutS: 2, Type: config.ModelTypeEmbedding, UpstreamModel: "embed-served", Pool: []config.Replica{replica}}
	cfg.Models["unreported"] = config.Model{Protocols: []string{config.ProtocolAnthropic}, TimeoutS: 3, IdleTimeoutS: 2, UpstreamModel: "not-served", Pool: []config.Replica{replica}}
	_, router := configured(t, cfg)

	resp, err := http.Get(router.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || resp.StatusCode != 200 || list.Object != "list" {
		t.Fatal(resp.StatusCode, err)
	}
	got := map[string]map[string]any{}
	for _, entry := range list.Data {
		got[entry["id"].(string)] = entry
		// Existing fields stay.
		if entry["object"] != "model" || entry["owned_by"] != "open-llm-router" || entry["created"] != float64(0) || entry["recipient_details"] == nil {
			t.Fatalf("existing fields changed: %v", entry)
		}
	}
	for _, tc := range []struct {
		id        string
		length    any // nil: omitted
		typ       string
		protocols []any
	}{
		{"chat-model", float64(32768), "chat", []any{"openai", "anthropic"}}, // smallest across replicas
		{"alias", float64(32768), "chat", []any{"openai", "anthropic"}},      // the target's values
		{"pinned", float64(8192), "chat", []any{"openai"}},                   // configured wins
		{"embed", float64(131072), "embedding", []any{"openai"}},             // upstream name looked up
		{"unreported", nil, "chat", []any{"anthropic"}},                      // unknown is omitted
	} {
		entry, ok := got[tc.id]
		if !ok {
			t.Fatalf("%s missing from %v", tc.id, list.Data)
		}
		length, present := entry["context_length"]
		if tc.length == nil && present || tc.length != nil && length != tc.length {
			t.Errorf("%s context_length = %v (present %v), want %v", tc.id, length, present, tc.length)
		}
		if entry["type"] != tc.typ || !reflect.DeepEqual(entry["protocols"], tc.protocols) {
			t.Errorf("%s type %v protocols %v", tc.id, entry["type"], entry["protocols"])
		}
	}
}
