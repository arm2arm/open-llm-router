package settings

import (
	"context"
	"crypto/sha256"
	"net/url"
	"regexp"
	"strings"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// Backend is a reusable origin. Only encrypted credential references are stored
// in routing documents; resolved API keys exist only in the runtime config.
type Backend struct {
	Name           string    `json:"name,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Groups         []string  `json:"groups"`
	Preset         string    `json:"preset,omitempty"`
	URL            string    `json:"url"`
	APIKey         SecretRef `json:"api_key"`
	PrivateNetwork bool      `json:"private_network"`
	HealthPath     string    `json:"health_path"`
}

var backendID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var storedID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NormalizeBackendURL accepts the common API base URLs pasted into the editor.
func NormalizeBackendURL(raw string) (string, error) {
	return config.NormalizeBackendURL(raw)
}

// SecretName binds a saved key to both backend identity and canonical origin.
// Copying a secret reference to a different URL cannot forward its credential.
func (b *Backend) SecretName(id string) string {
	origin, _ := NormalizeBackendURL(b.URL)
	hash := sha256.Sum256([]byte(id + "\x00" + origin))
	// Keep the existing managed_secrets setting-name grammar (letters and
	// underscores), using an injective a-p encoding of each digest nibble.
	name := []byte("backend_")
	for _, b := range hash {
		name = append(name, 'a'+(b>>4), 'a'+(b&15))
	}
	return string(name)
}

func (r *Routing) backendProblems() []string {
	var problems []string
	if len(r.Backends) > 256 {
		problems = append(problems, "backends: at most 256 backends")
	}
	for id, backend := range r.Backends {
		if !backendID.MatchString(id) || backend == nil {
			problems = append(problems, "backends: choose a name of 1 to 64 letters, digits, underscores or hyphens")
			continue
		}
		if backend.Name != "" && !plainText(backend.Name, 128) {
			problems = append(problems, "backends."+id+": choose a name up to 128 bytes")
		}
		if backend.Provider != "" && !plainText(backend.Provider, 64) {
			problems = append(problems, "backends."+id+": invalid detected provider")
		}
		if len(backend.Groups) > 256 {
			problems = append(problems, "backends."+id+": at most 256 groups")
		}
		for _, group := range backend.Groups {
			if _, ok := r.Groups[group]; !ok {
				problems = append(problems, "backends."+id+": choose existing groups")
			}
		}
		if backend.Preset != "" {
			if _, ok := r.BackendPresets[backend.Preset]; !ok {
				problems = append(problems, "backends."+id+": choose an existing preset")
			}
		}
		origin, err := NormalizeBackendURL(backend.URL)
		if err != nil || origin != backend.URL {
			problems = append(problems, "backends."+id+": use a canonical HTTP(S) host URL")
		}
		if backend.APIKey.Env != "" {
			problems = append(problems, "backends."+id+": enter an API key instead of an environment reference")
		}
		if backend.APIKey.Stored != "" && !storedID.MatchString(backend.APIKey.Stored) {
			problems = append(problems, "backends."+id+": invalid stored API key reference")
		}
		path, pathErr := url.ParseRequestURI(backend.HealthPath)
		if len(backend.HealthPath) > 256 || pathErr != nil || path.IsAbs() || !strings.HasPrefix(backend.HealthPath, "/") || strings.HasPrefix(backend.HealthPath, "//") || strings.ContainsAny(backend.HealthPath, "?#\r\n") {
			problems = append(problems, "backends."+id+": choose a health path starting with /")
		}
	}
	if len(r.BackendPresets) > 64 {
		problems = append(problems, "backend_presets: at most 64 presets")
	}
	if preset, ok := r.BackendPresets["unlimited"]; r.BackendPresets != nil && (!ok || preset.Limited()) {
		problems = append(problems, "backend_presets: the unlimited preset must exist without limits")
	}
	for id, preset := range r.BackendPresets {
		if !backendID.MatchString(id) || !plainText(preset.Name, 128) {
			problems = append(problems, "backend_presets: choose valid preset names")
		}
		if preset.RPM != nil && (*preset.RPM < 1 || *preset.RPM > 1000000000) || preset.TPM != nil && (*preset.TPM < 1 || *preset.TPM > 1000000000000) || preset.MaxConcurrent != nil && (*preset.MaxConcurrent < 1 || *preset.MaxConcurrent > 1000000) {
			problems = append(problems, "backend_presets."+id+": limits must be positive or unlimited")
		}
	}
	return problems
}

func ValidBackendKey(value string) bool { return validSecret([]byte(value)) }

// BackendToken resolves an origin-bound reference for administrator discovery.
func BackendToken(ctx context.Context, s *RoutingService, id string, backend *Backend) (string, error) {
	// Discovery does not change access or presets; validate connection fields.
	copy := *backend
	copy.Groups, copy.Preset = nil, ""
	doc := &Routing{Backends: map[string]*Backend{id: &copy}, BackendPresets: config.DefaultBackendPresets()}
	if problems := doc.backendProblems(); len(problems) != 0 {
		return "", &ValidationError{problems}
	}
	values, _, problems, err := s.resolve(ctx, s.DB, doc)
	if err != nil {
		return "", err
	}
	if len(problems) != 0 {
		return "", &ValidationError{problems}
	}
	return values[backend.SecretName(id)], nil
}
