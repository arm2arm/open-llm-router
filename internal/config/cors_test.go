package config

import (
	"strings"
	"testing"
)

func TestCORSConfiguration(t *testing.T) {
	cfg, err := Parse([]byte(valid), lookupIn(validEnv))
	if err != nil || len(cfg.CORS.AllowedOrigins) != 0 {
		t.Fatalf("CORS should be disabled by default: %v", err)
	}
	cfg, err = Parse([]byte(valid+`cors:
  allowed_origins: ["https://AI.AIP.DE:443/", "https://ai.aip.de", "http://localhost:5176", "http://[::1]:80"]
  extra_allowed_headers: [x-client-name, X-Client-Name]
`), lookupIn(validEnv))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.CORS.AllowedOrigins, ","); got != "https://ai.aip.de,http://localhost:5176,http://[::1]" {
		t.Fatal(got)
	}
	if got := strings.Join(cfg.CORS.ExtraAllowedHeaders, ","); got != "X-Client-Name" {
		t.Fatal(got)
	}
	for _, origin := range []string{"*", "null", "https://*.aip.de", "ai.aip.de", "https://user:password@ai.aip.de", "https://ai.aip.de/v1", "https://ai.aip.de/%2f", "https://ai.aip.de?", "https://ai.aip.de#", "https://ai.aip.de:0", "https://ai.aip.de:65536", "https://ai.aip.de:", "ftp://ai.aip.de", "https://ai.aip.de,evil.test", "https://[fe80::1%25eth0]", "https://ai.aip.de\r\nX-Test: yes"} {
		t.Run(origin, func(t *testing.T) {
			policy := CORS{AllowedOrigins: []string{origin}}
			if len(policy.validate(false)) == 0 {
				t.Fatal("accepted invalid origin")
			}
		})
	}
	for _, header := range []string{"*", "", "Bad Header", "X-Test\r\nInjected", strings.Repeat("X", 129)} {
		policy := CORS{ExtraAllowedHeaders: []string{header}}
		if len(policy.validate(false)) == 0 {
			t.Fatalf("accepted invalid extra header %q", header)
		}
	}
	policy := CORS{AllowedOrigins: []string{"http://ai.aip.de"}}
	if len(policy.validate(true)) == 0 {
		t.Fatal("accepted plaintext origin under require_tls")
	}
	policy = CORS{AllowedOrigins: make([]string, 129), ExtraAllowedHeaders: make([]string, 33)}
	if got := strings.Join([]string(policy.validate(false)), " "); !strings.Contains(got, "at most 128") || !strings.Contains(got, "at most 32") {
		t.Fatal("missing size bounds")
	}
	if _, err := Parse([]byte(valid+"cors: {allowed_origins: ['*']}\n"), lookupIn(validEnv)); err == nil {
		t.Fatal("startup accepted invalid CORS policy")
	}
}
