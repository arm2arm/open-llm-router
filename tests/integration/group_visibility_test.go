package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestPortalReceivedGroupsVisibleWithoutGrantingAccess(t *testing.T) {
	f := startPortal(t, usageOptions{portalConfig: func(c *config.Config) {
		c.AdminAPI.Enabled = true
		c.Groups["configured-empty"] = config.Group{Models: []string{}}
	}})
	admin, pierre, other := f.browser(t), f.browser(t), f.browser(t)
	admin.login(t, "root", []string{"admins"})
	id := pierre.login(t, "pierre", []string{"/ErUM", "ErUM"})
	otherID := other.login(t, "case-sensitive", []string{"erum"})
	accounts := auth.Admin{Pool: f.pool}
	if err := accounts.SetEnabled(t.Context(), otherID, false); err != nil {
		t.Fatal(err)
	}
	f.writer.Record(usage.Record{UserID: id, Model: "historical-model", StartedAt: time.Now(), Protocol: "openai", Status: "200", InputTokens: 3, OutputTokens: 7})
	waitFor(t, func() bool { return f.writer.Counters().Written == 1 })
	type group struct {
		Group      string
		Members    int
		Configured bool
		Admin      bool
		Requests   int `json:"requests_today"`
		Tokens     int `json:"tokens_today"`
	}
	read := func() map[string]group {
		t.Helper()
		response, data := admin.call(t, "GET", "/portal/api/admin/groups", "", "")
		var result struct{ Groups []group }
		if response.StatusCode != 200 || json.Unmarshal(data, &result) != nil {
			t.Fatal("group list failed", response.StatusCode)
		}
		out := map[string]group{}
		var names []string
		for _, g := range result.Groups {
			out[g.Group] = g
			names = append(names, g.Group)
		}
		if !slices.IsSorted(names) || len(out) != len(names) {
			t.Fatalf("group list is not sorted and unique: %q", names)
		}
		return out
	}
	groups := read()
	if got := groups["ErUM"]; got.Members != 1 || got.Configured || got.Admin || got.Requests != 1 || got.Tokens != 10 {
		t.Fatalf("received group was hidden or miscounted: %+v", got)
	}
	if got, exists := groups["erum"]; !exists || got.Members != 0 || got.Configured {
		t.Fatalf("disabled-only group or case was lost: %+v", got)
	}
	if got := groups["configured-empty"]; !got.Configured || got.Members != 0 {
		t.Fatal("empty configured policy was lost")
	}
	if got := groups["admins"]; !got.Configured || !got.Admin {
		t.Fatal("administrator policy changed")
	}
	response, data := admin.call(t, "GET", "/portal/api/admin/users/"+id, "", "")
	var detail struct {
		User   struct{ Groups []string }
		Models []string
		Admin  bool
	}
	if response.StatusCode != 200 || json.Unmarshal(data, &detail) != nil || !slices.Equal(detail.User.Groups, []string{"ErUM"}) || len(detail.Models) != 0 || detail.Admin {
		t.Fatal("discovery changed permissions or membership")
	}
	if response, _ := pierre.call(t, "GET", "/portal/api/admin/groups", "", ""); response.StatusCode != 403 {
		t.Fatal("non-administrator read group memberships")
	}
	if response, _ := admin.call(t, "POST", "/portal/api/admin/users", `{"username":"local-member","groups":["ErUM"],"password":"synthetic-local-password"}`, f.cfg.Portal.BaseURL); response.StatusCode != 400 {
		t.Fatal("unconfigured group became assignable to local users")
	}
	// The usage-only administration permission retains the configured-group
	// aggregate view; discovering membership names requires users:read.
	localAdmin := signInLocalAdmin(t, f)
	for _, permission := range []string{"usage:read", "users:read"} {
		body, _ := json.Marshal(map[string]any{"label": "group visibility", "permissions": []string{permission}, "allowed_networks": []string{"127.0.0.1"}, "expires_in_days": 1})
		response, data := localAdmin.call(t, "POST", "/portal/api/admin/agent-keys", string(body), f.cfg.Portal.BaseURL)
		var issued struct{ Key string }
		if response.StatusCode != 201 || json.Unmarshal(data, &issued) != nil {
			t.Fatal("administration key issuance failed")
		}
		operation := "get_usage"
		if permission == "users:read" {
			operation = "list_groups"
		}
		request, err := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/api/admin/v1/read/"+operation, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+issued.Key)
		request.Header.Set("Content-Type", "application/json")
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		data, err = io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || strings.Contains(string(data), `"group":"ErUM"`) != (permission == "users:read") {
			t.Fatal("group discovery did not respect administration permissions")
		}
	}
	// Memberships refresh on the next login. Once no user has an unconfigured
	// group, it no longer appears in the list.
	pierre.login(t, "pierre", nil)
	if _, exists := read()["ErUM"]; exists {
		t.Fatal("removed unconfigured membership remained in group discovery")
	}
}
