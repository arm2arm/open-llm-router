package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/pool"
)

type recipientRuntime struct{ cfg *config.Config }

func (r recipientRuntime) Snapshot() (*config.Config, *pool.Manager) { return r.cfg, nil }
func (recipientRuntime) PrepareRouting(context.Context, *config.Config, []string, bool) (func(), map[string][]pool.State, error) {
	panic("not used")
}

func TestCatalogueDisclosesActivePermittedRecipients(t *testing.T) {
	public := config.Replica{PublicOrganization: "Institute A", PrivacyURL: "https://a.example/privacy", URL: "http://internal-a", HealthPath: "/secret-path", BearerToken: "secret-key", BearerTokenEnv: "PRIVATE_TOKEN"}
	restricted := config.Replica{Backend: "private-backend", PublicOrganization: "Restricted institute", PrivacyURL: "https://restricted.example/privacy", URL: "http://internal-restricted"}
	active := &config.Config{
		Groups:   map[string]config.Group{"staff": {Models: []string{"mixed", "complete", "restricted"}}, "admins": {Admin: true}},
		Backends: map[string]config.BackendPolicy{"private-backend": {Groups: []string{}}},
		Models: map[string]config.Model{
			"mixed":      {Protocols: []string{"openai"}, Pool: []config.Replica{public, public, {PublicOrganization: "Institute B"}, {}, restricted}},
			"complete":   {Pool: []config.Replica{public, restricted}},
			"restricted": {Pool: []config.Replica{restricted}},
			"denied":     {Pool: []config.Replica{public}},
		},
		Aliases: map[string]string{"alias": "mixed", "denied-alias": "denied", "restricted-alias": "restricted"},
	}
	// No health manager is available. Disclosure must use the full active
	// configuration, even while every backend is unavailable.
	h := &Handler{cfg: &config.Config{Portal: config.Portal{BaseURL: "https://router.example", PrivacyURL: "https://site.example/privacy"}}, RoutingRuntime: recipientRuntime{active}}
	read := func(u user) []model {
		w := httptest.NewRecorder()
		h.models(w, httptest.NewRequest("GET", "/portal/api/models", nil), u)
		var body struct {
			Models     []model
			PrivacyURL string `json:"privacy_url"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.PrivacyURL != h.cfg.Portal.PrivacyURL {
			t.Fatal(w.Body.String())
		}
		for _, forbidden := range []string{"internal-", "secret-", "PRIVATE_TOKEN", "private-backend"} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatalf("catalogue exposed %s", forbidden)
			}
		}
		return body.Models
	}
	got := read(user{Groups: []string{"staff"}})
	wantRecipients := []policy.Recipient{{Organization: "Institute A", PrivacyURL: "https://a.example/privacy"}, {Organization: "Institute B"}, {}}
	if len(got) != 3 || got[0].ID != "alias" || got[1].ID != "complete" || got[2].ID != "mixed" || got[0].Canonical != "mixed" || !reflect.DeepEqual(got[0].Recipients, wantRecipients) || !reflect.DeepEqual(got[0].Recipients, got[2].Recipients) || got[0].RecipientsComplete || !got[1].RecipientsComplete {
		t.Fatalf("unexpected disclosure: %+v", got)
	}
	for _, u := range []user{{Groups: []string{"admins"}}, {ProtectedAdmin: true}} {
		got := read(u)
		if len(got) != 7 || len(got[0].Recipients) != 4 {
			t.Fatalf("admin recipients missing: %+v", got)
		}
	}
	// Replacing the runtime snapshot updates alias recipients without a restart.
	active.Aliases["alias"] = "complete"
	got = read(user{Groups: []string{"staff"}})
	if !got[0].RecipientsComplete || len(got[0].Recipients) != 1 {
		t.Fatal("stale routing disclosure")
	}
}

func TestSignInPrivacyMetadataOptional(t *testing.T) {
	for _, privacy := range []string{"", "https://site.example/privacy"} {
		cfg := &config.Config{Portal: config.Portal{BaseURL: "http://localhost:8080", PrivacyURL: privacy}}
		h, err := New(t.Context(), cfg, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.authMethods(w, httptest.NewRequest("GET", "/portal/api/auth", nil))
		var body struct {
			PrivacyURL string `json:"privacy_url"`
			Local      struct{ Available bool }
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.PrivacyURL != privacy || !body.Local.Available {
			t.Fatal(w.Body.String())
		}
	}
}
