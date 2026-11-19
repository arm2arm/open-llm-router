package integration_test

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/userdata"
)

func TestInstitutionalProfileLoginRefreshAndPrivacy(t *testing.T) {
	f := startPortal(t)
	b := f.browser(t)
	signIn := func(extra map[string]any) string {
		t.Helper()
		q, _ := b.challenge(t)
		code := f.provider.authorize(t, q, "profile-person", []string{"staff"}, func(claims map[string]any) {
			for key, value := range extra {
				claims[key] = value
			}
		})
		r, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
		if r.StatusCode != 303 {
			t.Fatal("profile sign-in failed", r.StatusCode)
		}
		return "profile-person"
	}
	signIn(map[string]any{"name": "Dr. Ada Lovelace", "given_name": "Ada", "family_name": "Lovelace", "institution": "Example Institute"})
	var id string
	assertProfile := func(want auth.Profile) {
		t.Helper()
		r, data := b.call(t, "GET", "/portal/api/me", "", "")
		var me struct {
			User struct {
				auth.Profile
				ID     string
				Groups []string
			}
			Admin bool
		}
		if r.StatusCode != 200 || json.Unmarshal(data, &me) != nil || me.User.Profile != want || me.Admin || len(me.User.Groups) != 1 || me.User.Groups[0] != "staff" {
			t.Fatalf("profile/grants mismatch: %d %s", r.StatusCode, data)
		}
		if id != "" && id != me.User.ID {
			t.Fatal("profile update changed identity")
		}
		id = me.User.ID
	}
	want := auth.Profile{FullName: "Dr. Ada Lovelace", GivenName: "Ada", FamilyName: "Lovelace", Institution: "Example Institute"}
	assertProfile(want)
	admin := f.browser(t)
	admin.login(t, "profile-reviewer", []string{"admins"})
	for _, query := range []string{"Ada Lovelace", "Example Institute"} {
		r, data := admin.call(t, "GET", "/portal/api/admin/users?match="+url.QueryEscape(query), "", "")
		var result struct {
			Users []struct {
				auth.Profile
				ID string
			}
		}
		if r.StatusCode != 200 || json.Unmarshal(data, &result) != nil || len(result.Users) != 1 || result.Users[0].ID != id || result.Users[0].Profile != want {
			t.Fatalf("admin profile search: %d %s", r.StatusCode, data)
		}
	}
	r, data := admin.call(t, "GET", "/portal/api/admin/users/"+id, "", "")
	var detail struct{ User auth.Profile }
	if r.StatusCode != 200 || json.Unmarshal(data, &detail) != nil || detail.User != want {
		t.Fatal("admin detail missing profile")
	}
	if r, _ := b.call(t, "GET", "/portal/api/admin/users/"+id, "", ""); r.StatusCode != 403 {
		t.Fatal("profile bypassed admin boundary")
	}
	var exported bytes.Buffer
	if err := userdata.Export(t.Context(), f.pool, id, &exported); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(exported.Bytes(), &detail) != nil || detail.User != want {
		t.Fatal("export omitted profile")
	}
	// Optional profile omissions and ordinary group/token synchronization must
	// retain names, even when their tokens only contain authorization claims.
	signIn(nil)
	assertProfile(want)
	if _, err := (auth.Admin{Pool: f.pool}).SyncIdentity(t.Context(), f.cfg.OIDC.Issuer, "profile-person", "profile-person", "profile-person@example.org", []string{"staff"}, false); err != nil {
		t.Fatal(err)
	}
	assertProfile(want)
	signIn(map[string]any{"name": "Ada Byron", "family_name": "Byron", "institution": "New Institute"})
	want.FullName, want.FamilyName, want.Institution = "Ada Byron", "Byron", "New Institute"
	assertProfile(want)
	// Same profile is a no-op: don't invalidate other administration previews.
	var before, after string
	if err := f.pool.QueryRow(t.Context(), `SELECT synced_at::text FROM users WHERE id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	signIn(map[string]any{"name": "Ada Byron", "given_name": "Ada", "family_name": "Byron", "institution": "New Institute"})
	if err := f.pool.QueryRow(t.Context(), `SELECT synced_at::text FROM users WHERE id=$1`, id).Scan(&after); err != nil || before != after {
		t.Fatal("identical profile caused a write", err)
	}
	signIn(map[string]any{"name": "", "given_name": "", "family_name": "", "institution": ""})
	assertProfile(auth.Profile{})
	if err := (auth.Admin{Pool: f.pool}).DeleteUser(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.call(t, "GET", "/portal/api/me", "", ""); r.StatusCode != 401 {
		t.Fatal("deleted profile remained accessible")
	}
}

func TestInstitutionalProfileCustomMapping(t *testing.T) {
	f := startPortal(t, usageOptions{portalConfig: func(c *config.Config) { c.OIDC.InstitutionClaim = "home_institution" }})
	b := f.browser(t)
	q, _ := b.challenge(t)
	code := f.provider.authorize(t, q, "mapped-person", []string{"staff"}, func(claims map[string]any) {
		claims["given_name"], claims["family_name"] = "Zoë", "Müller"
		claims["home_institution"] = []string{"Institute A", "Institute B"}
		claims["institution"] = "Must not override configured mapping"
	})
	r, _ := b.call(t, "GET", "/portal/callback?"+url.Values{"state": {q.Get("state")}, "code": {code}}.Encode(), "", "")
	if r.StatusCode != 303 {
		t.Fatal("mapped sign-in failed", r.StatusCode)
	}
	r, data := b.call(t, "GET", "/portal/api/me", "", "")
	if r.StatusCode != 200 || !strings.Contains(string(data), `"full_name":"Zoë Müller"`) || !strings.Contains(string(data), `"institution":"Institute A; Institute B"`) {
		t.Fatal("profile mapping failed", string(data))
	}
}
