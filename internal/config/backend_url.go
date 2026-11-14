package config

import (
	"errors"
	"net/url"
	"strings"
)

// NormalizeBackendURL converts a host or common API URL entered by an admin
// into an origin. Inference, health and discovery supply their own API paths.
// Deployment URLs remain strict origins; this is for managed editor input.
func NormalizeBackendURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.ContainsAny(raw, "?#") || u.RawPath != "" || len(raw) > 2048 {
		return "", errors.New("enter an HTTP(S) backend URL without credentials, query or fragment")
	}
	switch strings.TrimRight(u.Path, "/") {
	case "", "/v1", "/v1/models", "/v1/chat/completions", "/v1/messages", "/api/tags":
	default:
		return "", errors.New("enter the host URL or its /v1 API base")
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "http" && u.Port() == "80" || u.Scheme == "https" && u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u.String(), nil
}
