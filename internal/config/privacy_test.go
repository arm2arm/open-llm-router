package config

import (
	"strings"
	"testing"
)

func TestPrivacyURLs(t *testing.T) {
	for _, raw := range []string{"", "https://example.org/privacy", "http://localhost:8080/privacy?lang=en#rights", "https://example.org/notice%20en"} {
		cfg, err := Parse([]byte("models: {}"), lookupIn(map[string]string{"PORTAL_PRIVACY_URL": raw}))
		if err != nil || cfg.Portal.PrivacyURL != raw {
			t.Fatalf("valid privacy URL rejected: %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"javascript:alert(1)", "data:text/html,test", "//example.org/privacy", "/privacy", "https://user:secret@example.org", "https:///privacy", "https://example.org\\@evil.org", "https://example.org/\nsecret", "https://example.org/ secret", "https://example.org:0", "https://example.org:99999", "https://example.org:", strings.Repeat("x", 2049)} {
		_, err := Parse([]byte("models: {}"), lookupIn(map[string]string{"PORTAL_PRIVACY_URL": raw}))
		if err == nil || !strings.Contains(err.Error(), "PORTAL_PRIVACY_URL") || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe URL accepted or echoed: %q: %v", raw, err)
		}
	}
}

func TestReplicaPublicDisclosureValidation(t *testing.T) {
	for _, tc := range []struct {
		fields string
		valid  bool
	}{
		{`public_organization: Research Institute, privacy_url: https://example.org/privacy`, true},
		{`public_organization: '', privacy_url: ''`, true},
		{`public_organization: '   '`, false},
		{`public_organization: "bad\nname"`, false},
		{`privacy_url: 'javascript:alert(1)'`, false},
	} {
		data := strings.Replace(valid, "url: http://backend-a:8000,", tc.fields+", url: http://backend-a:8000,", 1)
		_, err := Parse([]byte(data), lookupIn(validEnv))
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.fields, err)
		}
	}
}
