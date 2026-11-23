package portal

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/pool"
)

// Embed the unused mutation methods. A single response must read one snapshot
// for model permissions, aliases and eligible backend recipients together.
type disclosureSnapshot struct {
	RoutingRuntime
	cfg   *config.Config
	reads int
}

func (s *disclosureSnapshot) Snapshot() (*config.Config, *pool.Manager) {
	s.reads++
	return s.cfg, nil
}

func TestPortalDisclosesOnlyEligibleModelRecipients(t *testing.T) {
	base := &config.Config{Portal: config.Portal{
		BaseURL: "https://router.example", PrivacyNoticeURL: "https://site.example/privacy",
		RecipientLabels: map[string]string{
			"https://primary.private": "Site B", "https://replica.private": "Site A",
			"https://another.private": "Site A", "https://restricted.private": "Restricted site",
		},
	}}
	h, err := New(t.Context(), base, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// The public sign-in response must reveal only the operator's notice URL.
	public := httptest.NewRecorder()
	h.ServeHTTP(public, httptest.NewRequest("GET", "/portal/api/auth", nil))
	if public.Code != 200 || !strings.Contains(public.Body.String(), base.Portal.PrivacyNoticeURL) ||
		strings.Contains(public.Body.String(), "Site A") || strings.Contains(public.Body.String(), ".private") {
		t.Fatalf("unexpected public disclosure: %s", public.Body)
	}
	live := *base
	live.Groups = map[string]config.Group{"members": {Models: []string{"test", "unknown"}}}
	live.Backends = map[string]config.BackendPolicy{"private": {Groups: []string{"admins"}}}
	live.Models = map[string]config.Model{
		"test": {Protocols: []string{"openai"}, Pool: []config.Replica{
			{URL: "https://primary.private"}, {URL: "https://another.private"},
			{URL: "https://replica.private"}, {Backend: "private", URL: "https://restricted.private"},
			{Backend: "private", URL: "https://unlabelled-denied.private"},
		}},
		"unknown":      {Pool: []config.Replica{{URL: "https://new.private", BearerToken: "backend-credential"}}},
		"hidden-model": {Pool: []config.Replica{{URL: "https://restricted.private"}}},
	}
	live.Aliases = map[string]string{"alias": "test", "hidden-alias": "hidden-model"}
	runtime := &disclosureSnapshot{cfg: &live}
	h.RoutingRuntime = runtime
	w := httptest.NewRecorder()
	h.models(w, httptest.NewRequest("GET", "/portal/api/models", nil), user{Groups: []string{"members"}})
	var got struct {
		Models  []model `json:"models"`
		Privacy string  `json:"privacy_notice_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if runtime.reads != 1 || got.Privacy != base.Portal.PrivacyNoticeURL || len(got.Models) != 3 {
		t.Fatalf("unexpected model disclosure: %s", w.Body)
	}
	for _, entry := range got.Models {
		switch entry.ID {
		case "test", "alias":
			if entry.Canonical != "test" || !slices.Equal(entry.Recipients, []string{"Site A", "Site B"}) || entry.HasUnlistedRecipients {
				t.Fatalf("incorrect eligible destinations: %+v", entry)
			}
			if len(entry.RecipientDetails) != 2 || entry.RecipientsComplete {
				t.Fatalf("legacy labels must retain details but report missing privacy URLs: %+v", entry)
			}
		case "unknown":
			if entry.Recipients == nil || len(entry.Recipients) != 0 || !entry.HasUnlistedRecipients {
				t.Fatalf("missing warning for unlisted recipient: %+v", entry)
			}
		default:
			t.Fatalf("inaccessible model: %+v", entry)
		}
	}
	for _, private := range []string{".private", "Restricted site", "backend-credential", "hidden-model", "hidden-alias"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("model catalog exposed %q", private)
		}
	}
}
