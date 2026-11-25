package modelinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/pool"
)

func catalog(t *testing.T, body string) string {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pool.CatalogPath {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(backend.Close)
	return backend.URL
}

func TestMetadata(t *testing.T) {
	configured := 16384
	replica := func(url string) config.Replica { return config.Replica{URL: url, Weight: 1, MaxInflight: 1} }
	reports := catalog(t, `{"data":[{"id":"chat","max_model_len":131072},{"id":"pinned","max_model_len":131072},{"id":"embed","max_model_len":8192}]}`)
	smaller := catalog(t, `{"data":[{"id":"chat","max_model_len":65536}]}`)
	silent := catalog(t, `{"data":[{"id":"unknown"}]}`)
	cfg := &config.Config{
		Models: map[string]config.Model{
			"chat":    {Protocols: []string{config.ProtocolOpenAI, config.ProtocolAnthropic}, Pool: []config.Replica{replica(reports), replica(smaller)}},
			"pinned":  {Protocols: []string{config.ProtocolOpenAI}, ContextLength: &configured, Pool: []config.Replica{replica(reports)}},
			"embed":   {Protocols: []string{config.ProtocolOpenAI}, Type: config.ModelTypeEmbedding, Pool: []config.Replica{replica(reports)}},
			"unknown": {Protocols: []string{config.ProtocolOpenAI}, Pool: []config.Replica{replica(silent)}},
		},
		Aliases: map[string]string{"default": "chat", "small": "pinned"},
	}
	pools := pool.New(cfg, http.DefaultTransport)
	pools.Check(t.Context())
	for _, tc := range []struct {
		name, canonical string
		want            Metadata
	}{
		// Detected: the smallest value across the replicas.
		{"chat", "chat", Metadata{ContextLength: 65536, Type: "chat", Protocols: []string{"openai", "anthropic"}}},
		// A configured value wins over a detected one.
		{"pinned", "pinned", Metadata{ContextLength: 16384, Type: "chat", Protocols: []string{"openai"}}},
		{"embed", "embed", Metadata{ContextLength: 8192, Type: "embedding", Protocols: []string{"openai"}}},
		// Unknown stays 0, which JSON omits.
		{"unknown", "unknown", Metadata{Type: "chat", Protocols: []string{"openai"}}},
		// Aliases report their target's values.
		{"default", "chat", Metadata{ContextLength: 65536, Type: "chat", Protocols: []string{"openai", "anthropic"}}},
		{"small", "pinned", Metadata{ContextLength: 16384, Type: "chat", Protocols: []string{"openai"}}},
	} {
		canonical, got, ok := Describe(cfg, pools, tc.name)
		if !ok || canonical != tc.canonical || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %s %+v %v, want %s %+v", tc.name, canonical, got, ok, tc.canonical, tc.want)
		}
	}
	if _, _, ok := Describe(cfg, pools, "missing"); ok {
		t.Fatal("unknown model described")
	}
	// Without pools only configured values are known.
	if _, got, _ := Describe(cfg, nil, "chat"); got.ContextLength != 0 {
		t.Fatalf("context length without pools: %+v", got)
	}
	if _, got, _ := Describe(cfg, nil, "small"); got.ContextLength != configured {
		t.Fatalf("configured context length without pools: %+v", got)
	}
	// The returned protocols are a copy.
	_, got, _ := Describe(cfg, pools, "chat")
	got.Protocols[0] = "changed"
	if cfg.Models["chat"].Protocols[0] != config.ProtocolOpenAI {
		t.Fatal("metadata aliases the configuration")
	}
}

func TestEntryJSON(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.Model{
			"chat":  {Protocols: []string{config.ProtocolOpenAI}, Pool: []config.Replica{{URL: "http://backend", PublicOrganization: "Institute", PrivacyURL: "https://institute.example/privacy"}}},
			"bare":  {},
			"embed": {Protocols: []string{config.ProtocolOpenAI}, Type: config.ModelTypeEmbedding},
		},
		Aliases: map[string]string{"alias": "chat"},
	}
	length := 4096
	model := cfg.Models["chat"]
	model.ContextLength = &length
	cfg.Models["chat"] = model
	entry, ok := NewEntry(cfg, nil, "alias", nil, false)
	if !ok {
		t.Fatal("alias entry not built")
	}
	encoded, _ := json.Marshal(entry)
	const want = `{"id":"alias","object":"model","created":0,"owned_by":"open-llm-router","context_length":4096,"type":"chat","protocols":["openai"],"recipient_details":[{"organization":"Institute","privacy_url":"https://institute.example/privacy"}],"recipients_complete":true}`
	if string(encoded) != want {
		t.Fatalf("entry\n%s\nwant\n%s", encoded, want)
	}
	entry, _ = NewEntry(cfg, nil, "bare", nil, false)
	encoded, _ = json.Marshal(entry)
	if strings.Contains(string(encoded), "context_length") || !strings.Contains(string(encoded), `"type":"chat","protocols":[]`) {
		t.Fatalf("model without metadata: %s", encoded)
	}
	entry, _ = NewEntry(cfg, nil, "embed", nil, false)
	if entry.Type != config.ModelTypeEmbedding {
		t.Fatalf("embedding type lost: %+v", entry)
	}
	if _, ok := NewEntry(cfg, nil, "missing", nil, false); ok {
		t.Fatal("entry built for an unknown model")
	}
}
