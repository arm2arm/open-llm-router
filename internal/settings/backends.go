package settings

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Backend is a reusable origin. Only encrypted credential references are stored
// in routing documents; resolved API keys exist only in the runtime config.
type Backend struct {
	URL            string    `json:"url"`
	APIKey         SecretRef `json:"api_key"`
	PrivateNetwork bool      `json:"private_network"`
	HealthPath     string    `json:"health_path"`
}

var backendID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var storedID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NormalizeBackendURL accepts the common API base URLs pasted into the editor.
func NormalizeBackendURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(raw) > 2048 {
		return "", errors.New("enter an HTTP(S) backend URL without credentials, query or fragment")
	}
	switch strings.TrimRight(u.Path, "/") {
	case "", "/v1", "/v1/models", "/v1/chat/completions", "/v1/messages", "/api/tags":
	default:
		return "", errors.New("enter the host URL or its /v1 API base")
	}
	u.Path, u.RawPath = "", ""
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "http" && u.Port() == "80" || u.Scheme == "https" && u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u.String(), nil
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
	return problems
}

func ValidBackendKey(value string) bool { return validSecret([]byte(value)) }

// BackendToken resolves an origin-bound reference for administrator discovery.
func BackendToken(ctx context.Context, s *RoutingService, id string, backend *Backend) (string, error) {
	doc := &Routing{Backends: map[string]*Backend{id: backend}}
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
