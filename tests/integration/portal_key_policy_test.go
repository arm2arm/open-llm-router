package integration_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestPortalKeyLifetimeChoicesMatchEnforcedPolicy(t *testing.T) {
	for _, maximum := range []int{30, 90, 120} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			f := startPortal(t, usageOptions{portalConfig: func(cfg *config.Config) {
				cfg.Auth.MaxKeyLifetimeDays = maximum
				cfg.Auth.DefaultKeyLifetimeDays = min(90, maximum)
			}})
			browser := f.browser(t)
			browser.login(t, "expiry-policy", []string{"staff"})
			response, data := browser.call(t, "GET", "/portal/api/keys", "", "")
			var listed struct {
				Policy struct {
					Default int `json:"default_lifetime_days"`
					Max     int `json:"max_lifetime_days"`
				}
			}
			if response.StatusCode != 200 || json.Unmarshal(data, &listed) != nil || listed.Policy.Max != maximum || listed.Policy.Default != min(90, maximum) {
				t.Fatalf("incorrect policy metadata: %d %s", response.StatusCode, data)
			}
			for _, days := range []int{1, 7, 30, 90, 365, maximum} {
				response, data = browser.call(t, "POST", "/portal/api/keys", fmt.Sprintf(`{"label":"expiry-%d","expires_in_days":%d}`, days, days), f.cfg.Portal.BaseURL)
				if days > maximum {
					if response.StatusCode != 400 || !strings.Contains(string(data), fmt.Sprintf("between 1 and %d days", maximum)) {
						t.Fatalf("missing policy limit: %d %s", response.StatusCode, data)
					}
				} else if response.StatusCode != 201 {
					t.Fatalf("offered lifetime %d rejected: %d %s", days, response.StatusCode, data)
				}
			}
		})
	}
}
