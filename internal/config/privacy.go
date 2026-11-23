package config

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidPrivacyURL checks a public link, never an inference destination. Empty
// links keep installations without a published notice usable. No URL is fetched.
func ValidPrivacyURL(raw string) bool {
	if raw == "" {
		return true
	}
	if len(raw) > 2048 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\") || strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return !strings.HasSuffix(u.Host, ":")
}

func ValidPublicOrganization(name string) bool {
	return name == "" || (strings.TrimSpace(name) == name && len(name) <= 128 && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl))
}
