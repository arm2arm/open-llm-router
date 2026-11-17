package integration_test

import (
	"net/url"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestCanonicalPublicOriginAllowsLoginAndMutation(t *testing.T) {
	for _, raw := range []string{"https://LLM.example.org:443/", "http://LLM.example.org:80", "https://LLM.example.org:8443"} {
		t.Run(raw, func(t *testing.T) {
			parsed, err := config.Parse([]byte("models: {}\ngroups: {}\n"), func(key string) (string, bool) { return raw, key == "PORTAL_BASE_URL" })
			if err != nil {
				t.Fatal(err)
			}
			f := startPortal(t, usageOptions{portalConfig: func(cfg *config.Config) { cfg.Portal.BaseURL = parsed.Portal.BaseURL }})
			browser := f.browser(t)
			challenge, _ := browser.challenge(t)
			callback, err := url.Parse(challenge.Get("redirect_uri"))
			if err != nil || callback.Scheme+"://"+callback.Host != parsed.Portal.BaseURL {
				t.Fatal("login did not use canonical public origin", err)
			}
			browser.login(t, "canonical-origin", []string{"staff"})
			resp, _ := browser.call(t, "POST", "/portal/logout", "{}", parsed.Portal.BaseURL)
			if resp.StatusCode != 200 {
				t.Fatal("canonical browser mutation rejected", resp.StatusCode)
			}
		})
	}
}
