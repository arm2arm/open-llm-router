package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/adminaccess"
	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPortalKeyChurnHasBoundedHistory(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	owner := b.login(t, "key-churn", []string{"staff"})
	throttled := 0
	for range 100 {
		response, data := b.call(t, "POST", "/portal/api/keys", `{"label":"churn fixture"}`, f.cfg.Portal.BaseURL)
		if response.StatusCode == http.StatusTooManyRequests {
			throttled++
			if response.Header.Get("Retry-After") != "60" {
				t.Fatal("missing retry guidance")
			}
			continue
		}
		var issued struct{ Key string }
		if response.StatusCode != 201 || json.Unmarshal(data, &issued) != nil || len(issued.Key) < 12 {
			t.Fatal("key issuance failed", response.StatusCode)
		}
		var key string
		if err := f.pool.QueryRow(t.Context(), `SELECT id::text FROM api_keys WHERE user_id=$1 AND prefix=$2`, owner, issued.Key[:12]).Scan(&key); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			response, _ = b.call(t, "DELETE", "/portal/api/keys/"+key, `{}`, f.cfg.Portal.BaseURL)
			if response.StatusCode != 204 {
				t.Fatal("revoke/delete failed before budget exhausted", response.StatusCode)
			}
		}
	}
	var events int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE actor_id=$1 AND action LIKE 'key.%'`, owner).Scan(&events); err != nil || events != 30 || throttled != 90 {
		t.Fatal("key churn exceeded bounded history", events, throttled, err)
	}
	other := f.browser(t)
	other.login(t, "another-key-owner", []string{"staff"})
	response, _ := other.call(t, "POST", "/portal/api/keys", `{"label":"unaffected user"}`, f.cfg.Portal.BaseURL)
	if response.StatusCode != 201 {
		t.Fatal("one user exhausted another user's budget", response.StatusCode)
	}
}

func TestMissingSelfServiceKeyDoesNotCreatePermanentHistory(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	owner := b.login(t, "missing-key", []string{"staff"})
	for i := range 100 {
		response, _ := b.call(t, "DELETE", "/portal/api/keys/00000000-0000-0000-0000-000000000001", `{}`, f.cfg.Portal.BaseURL)
		want := 404
		if i >= 30 {
			want = 429
		}
		if response.StatusCode != want {
			t.Fatal("missing key response", i, response.StatusCode)
		}
	}
	var events int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE actor_id=$1 AND action LIKE 'key.%'`, owner).Scan(&events); err != nil || events != 0 {
		t.Fatal("missing keys filled permanent history", events, err)
	}
}

func TestDeniedAgentSourcesHavePerCredentialAuditBudget(t *testing.T) {
	f := startPortal(t, usageOptions{portal: func(_ *portal.Handler, cfg *config.Config, _ *pgxpool.Pool, _ *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
		cfg.AdminAPI.TrustedProxyCIDRs = []string{"127.0.0.1/32"}
	}})
	owner := f.browser(t).login(t, "denied-agent-sources", []string{"admins"})
	key, secret, err := adminaccess.Issue(t.Context(), f.pool, owner, adminaccess.Create{Label: "restricted source", Permissions: []string{"service:read"}, Networks: []string{"10.0.0.0/8"}, Days: 1}, nil, 10, audit.Actor{Type: "portal_user", ID: owner})
	if err != nil {
		t.Fatal(err)
	}
	call := func(source string, want int) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "GET", f.router.URL+"/api/admin/v1/identity", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-Forwarded-For", source)
		response, err := f.router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatal("source restriction/budget", source, response.StatusCode)
		}
	}
	for i := range 100 {
		want := 403
		if i >= 5 {
			want = 429
		}
		call(fmt.Sprintf("203.0.113.%d", i+1), want)
	}
	var events int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='agent.access' AND result='refused' AND details#>>'{agent,key_id}'=$1`, key.ID).Scan(&events); err != nil || events != 5 {
		t.Fatal("source rotation amplified audit history", events, err)
	}
	call("10.2.3.4", 200)
}
