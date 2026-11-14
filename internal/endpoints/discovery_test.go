package endpoints

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
)

func discoveryService(t *testing.T, handlers ...http.HandlerFunc) (*Service, []ModelSource) {
	t.Helper()
	cfg := &config.Config{Models: map[string]config.Model{"chat": {}}}
	sources := []ModelSource{}
	model := cfg.Models["chat"]
	for _, handler := range handlers {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		model.Pool = append(model.Pool, config.Replica{URL: server.URL, BearerTokenEnv: "GPU_TOKEN", BearerToken: "synthetic-secret"})
		sources = append(sources, ModelSource{URL: server.URL, BearerTokenEnv: "GPU_TOKEN"})
	}
	cfg.Models["chat"] = model
	policy, err := backendpolicy.New(cfg, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Base: cfg, Policy: policy}, sources
}

func TestDiscoveryIntersectsCatalogsWithoutActivating(t *testing.T) {
	s, sources := discoveryService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic-secret" || r.Header.Get("Cookie") != "" {
			t.Error("catalog request isolation failed")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"z"},{"id":"a"},{"id":"a"}]}`))
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(404)
			return
		}
		if r.URL.Path != "/api/tags" {
			t.Error("wrong fallback")
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"a"},{"model":"b"}]}`))
	})
	result, err := s.Discover(t.Context(), "chat", sources)
	if err != nil || !result.Complete || !reflect.DeepEqual(result.Models, []string{"a"}) || result.Replicas[1].Source != "/api/tags" {
		t.Fatalf("unexpected catalog: %+v %v", result, err)
	}
	if len(result.Replicas[0].Models) != 2 {
		t.Fatal("not deduplicated")
	}
}

func TestDiscoveryFailureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"auth", "synthetic-secret", 401}, {"redirect", "", 302},
		{"malformed", "<html>synthetic-secret</html>", 200},
		{"pagination", `{"data":[{"id":"a"}],"has_more":true}`, 200},
		{"large", strings.Repeat("x", (1<<20)+1), 200},
		{"invalid name", `{"data":[{"id":"bad\nname"}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s, sources := discoveryService(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "http://127.0.0.1:1/control")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			result, err := s.Discover(t.Context(), "chat", sources)
			if err != nil || result.Complete || len(result.Models) != 0 || result.Replicas[0].Error == "" || calls.Load() != 1 {
				t.Fatalf("unsafe failure: %+v %v calls=%d", result, err, calls.Load())
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "synthetic-secret") {
				t.Fatal("backend body leaked")
			}
		})
	}
	var calls atomic.Int32
	s, sources := discoveryService(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"data":[]}`)) })
	bad := append(sources, ModelSource{URL: "http://127.0.0.1:1"})
	if _, err := s.Discover(t.Context(), "chat", bad); err == nil || calls.Load() != 0 {
		t.Fatal("unsafe destination probed")
	}
	bad = append([]ModelSource(nil), sources...)
	bad[0].BearerTokenEnv = "DATABASE_PASSWORD"
	if _, err := s.Discover(t.Context(), "chat", bad); err == nil || calls.Load() != 0 {
		t.Fatal("arbitrary secret reference accepted")
	}
	result, err := s.Discover(t.Context(), "chat", sources)
	if err != nil || !result.Complete || len(result.Models) != 0 {
		t.Fatal("empty catalog rejected")
	}
}

func TestDiscoveryPartialFailureOffersNoSharedModels(t *testing.T) {
	s, sources := discoveryService(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[{"id":"a"}]}`)) }, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	result, err := s.Discover(t.Context(), "chat", sources)
	if err != nil || result.Complete || len(result.Models) != 0 || len(result.Replicas[0].Models) != 1 {
		t.Fatal("partial catalog offered unsafe model", result, err)
	}
}

func TestNewBackendDetection(t *testing.T) {
	for _, provider := range []string{"vLLM", "SGLang", "Ollama", "OpenAI-compatible"} {
		t.Run(provider, func(t *testing.T) {
			s, sources := discoveryService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer new-key" {
					t.Error("key missing")
				}
				if r.URL.Path == "/api/tags" {
					if provider == "Ollama" {
						_, _ = w.Write([]byte(`{"models":[{"name":"deepseek-r1:8b"}]}`))
						return
					}
					http.NotFound(w, r)
					return
				}
				owner := strings.ToLower(provider)
				_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-r1:8b","owned_by":"` + owner + `"}]}`))
			})
			delete(s.Base.Models, "chat") // detection must work before a route exists
			result, err := s.Detect(t.Context(), sources[0], "new-key")
			if err != nil || result.Error != "" || result.Provider != provider || len(result.Models) != 1 || result.HealthPath == "" {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}
