package endpoints

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
)

// ModelSource deliberately accepts credential references, never credentials.
type ModelSource struct {
	URL            string `json:"url"`
	BearerTokenEnv string `json:"bearer_token_env"`
	PrivateNetwork bool   `json:"private_network,omitempty"`
}
type Catalog struct {
	URL        string   `json:"url"`
	Models     []string `json:"models"`
	Source     string   `json:"source,omitempty"`
	Error      string   `json:"error,omitempty"`
	Provider   string   `json:"provider,omitempty"`
	HealthPath string   `json:"health_path,omitempty"`
}
type Discovery struct {
	Models   []string  `json:"models"`
	Complete bool      `json:"complete"`
	Replicas []Catalog `json:"replicas"`
}

// Discover never publishes configuration or touches inference admission/health.
// Only models present on every replica are safe choices for the shared name.
func (s *Service) Discover(ctx context.Context, name string, sources []ModelSource) (Discovery, error) {
	result := Discovery{Models: []string{}, Replicas: []Catalog{}}
	if _, ok := s.Base.Models[name]; !ok {
		return result, ErrModel
	}
	if len(sources) == 0 || len(sources) > 32 {
		return result, &ValidationError{"choose between 1 and 32 replicas"}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	sources = slices.Clone(sources)
	seen := map[string]bool{}
	// Validate all destinations before making any catalog request.
	for i := range sources {
		source := &sources[i]
		source.URL = strings.TrimSuffix(strings.TrimSpace(source.URL), "/")
		if err := s.Policy.CheckBackend(ctx, source.URL, source.PrivateNetwork); err != nil {
			return result, &ValidationError{err.Error()}
		}
		if seen[source.URL] {
			return result, &ValidationError{"replica origins must be unique"}
		}
		seen[source.URL] = true
		if source.BearerTokenEnv != "" {
			if _, ok := s.Policy.Token(source.BearerTokenEnv); !ok {
				return result, &ValidationError{"choose an available backend credential reference"}
			}
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = s.Policy.DialContext
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result.Replicas = make([]Catalog, len(sources))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, source := range sources {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				result.Replicas[i] = Catalog{URL: source.URL, Models: []string{}, Error: "model discovery timed out"}
				return
			}
			token, _ := s.Policy.Token(source.BearerTokenEnv)
			requestCtx := ctx
			if source.PrivateNetwork {
				requestCtx = backendpolicy.WithPrivateNetwork(ctx, source.URL)
			}
			result.Replicas[i] = catalog(requestCtx, client, source.URL, token)
		})
	}
	wg.Wait()
	result.Complete = true
	for i, replica := range result.Replicas {
		if replica.Error != "" {
			result.Complete = false
			continue
		}
		if i == 0 {
			result.Models = slices.Clone(replica.Models)
		} else {
			result.Models = slices.DeleteFunc(result.Models, func(model string) bool { _, ok := slices.BinarySearch(replica.Models, model); return !ok })
		}
	}
	if !result.Complete {
		result.Models = []string{}
	}
	return result, nil
}

func catalog(ctx context.Context, client *http.Client, origin, token string) Catalog {
	return catalogPaths(ctx, client, origin, token, []string{"/v1/models", "/api/tags"})
}

func catalogPaths(ctx context.Context, client *http.Client, origin, token string, paths []string) Catalog {
	result := Catalog{URL: origin, Models: []string{}}
	for _, path := range paths {
		req, err := http.NewRequestWithContext(ctx, "GET", origin+path, nil)
		if err != nil {
			result.Error = "invalid backend origin"
			return result
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			result.Error = "model catalog request failed or timed out"
			return result
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		resp.Body.Close()
		if path == "/v1/models" && (resp.StatusCode == 404 || resp.StatusCode == 405 || resp.StatusCode == 501) {
			continue
		}
		if resp.StatusCode != 200 {
			result.Error = fmt.Sprintf("model catalog returned HTTP %d", resp.StatusCode)
			return result
		}
		if readErr != nil || len(data) > 1<<20 {
			result.Error = "model catalog is unreadable or exceeds 1 MiB"
			return result
		}
		var body struct {
			Data *[]struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
			Models *[]struct {
				Name  string `json:"name"`
				Model string `json:"model"`
			} `json:"models"`
			HasMore bool `json:"has_more"`
		}
		if json.Unmarshal(data, &body) != nil || body.HasMore || (path == "/v1/models" && body.Data == nil) || (path == "/api/tags" && body.Models == nil) {
			result.Error = "unsupported or paginated model catalog; enter the model name manually"
			return result
		}
		ids := []string{}
		if path == "/v1/models" {
			result.Provider = "OpenAI-compatible"
			for _, model := range *body.Data {
				ids = append(ids, model.ID)
				switch strings.ToLower(model.OwnedBy) {
				case "vllm":
					result.Provider = "vLLM"
				case "sglang":
					result.Provider = "SGLang"
				}
			}
		} else {
			result.Provider = "Ollama"
			for _, model := range *body.Models {
				id := model.Name
				if id == "" {
					id = model.Model
				}
				ids = append(ids, id)
			}
		}
		if len(ids) > 2048 {
			result.Error = "model catalog exceeds 2048 entries"
			return result
		}
		for _, id := range ids {
			if id == "" || len(id) > 256 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
				result.Error = "model catalog contains an invalid model name"
				return result
			}
		}
		slices.Sort(ids)
		result.Models, result.Source = slices.Compact(ids), path
		result.HealthPath = path
		return result
	}
	return result
}

// Detect probes one new backend without requiring an existing logical model.
// Redirects are refused and only the explicitly selected origin receives a key.
func (s *Service) Detect(ctx context.Context, source ModelSource, token string) (Catalog, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := s.Policy.CheckBackend(ctx, source.URL, source.PrivateNetwork); err != nil {
		return Catalog{}, &ValidationError{err.Error()}
	}
	if source.PrivateNetwork {
		ctx = backendpolicy.WithPrivateNetwork(ctx, source.URL)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy, transport.DialContext = nil, s.Policy.DialContext
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	result := catalog(ctx, client, source.URL, token)
	u, _ := url.Parse(source.URL)
	if result.Error == "" && u.Hostname() == "api.deepseek.com" {
		result.Provider = "DeepSeek"
	}
	if result.Error == "" && result.Provider == "OpenAI-compatible" {
		if ollama := catalogPaths(ctx, client, source.URL, token, []string{"/api/tags"}); ollama.Error == "" && ollama.Source != "" {
			result = ollama
		}
	}
	return result, nil
}
