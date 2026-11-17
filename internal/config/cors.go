package config

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// CORS allows personal browser clients to call /v1 with their own API keys.
// An empty allowlist disables CORS. Portal and administration APIs are excluded.
type CORS struct {
	AllowedOrigins      []string `yaml:"allowed_origins"`
	ExtraAllowedHeaders []string `yaml:"extra_allowed_headers"`
}

var corsHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (c *CORS) validate(requireTLS bool) Problems {
	var p Problems
	if len(c.AllowedOrigins) > 128 {
		p = append(p, "cors.allowed_origins: at most 128 origins")
	}
	origins := make([]string, 0, len(c.AllowedOrigins))
	seen := map[string]bool{}
	for i, raw := range c.AllowedOrigins {
		origin, ok := corsOrigin(raw)
		if !ok {
			p = append(p, fmt.Sprintf("cors.allowed_origins[%d]: must be an explicit HTTP(S) origin without credentials, path, query, fragment or wildcard", i))
			continue
		}
		if requireTLS && !strings.HasPrefix(origin, "https://") {
			p = append(p, "security.require_tls: CORS origins require HTTPS")
		}
		if !seen[origin] {
			origins = append(origins, origin)
			seen[origin] = true
		}
	}
	if len(c.ExtraAllowedHeaders) > 32 {
		p = append(p, "cors.extra_allowed_headers: at most 32 headers")
	}
	headers := make([]string, 0, len(c.ExtraAllowedHeaders))
	seen = map[string]bool{}
	for i, header := range c.ExtraAllowedHeaders {
		if len(header) > 128 || !corsHeaderName.MatchString(header) || header == "*" {
			p = append(p, fmt.Sprintf("cors.extra_allowed_headers[%d]: must be an explicit HTTP header name", i))
			continue
		}
		header = http.CanonicalHeaderKey(header)
		if !seen[header] {
			headers = append(headers, header)
			seen[header] = true
		}
	}
	// Allocate fresh slices: managed routing revisions shallow-copy operator settings.
	c.AllowedOrigins, c.ExtraAllowedHeaders = origins, headers
	return p
}

func corsOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || strings.ContainsAny(raw, "?#") || u.User != nil || u.Opaque != "" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", false
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if (scheme != "http" && scheme != "https") || host == "" {
		return "", false
	}
	if strings.Contains(host, ":") {
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Zone() != "" {
			return "", false
		}
		host = "[" + addr.String() + "]"
	} else {
		for _, ch := range host {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '-' || ch == '_') {
				return "", false
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", false
		}
		if !(scheme == "https" && n == 443 || scheme == "http" && n == 80) {
			host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(n))
		}
	}
	return scheme + "://" + host, true
}
