package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/settings"
)

func TestRoutingImportAndDraftShareDocumentLimit(t *testing.T) {
	var input strings.Builder
	input.WriteString("models:\n")
	for i := range 128 {
		fmt.Fprintf(&input, "  m%03d:\n    protocols: [openai]\n    timeout_s: 5\n    pool:\n", i)
		for j := range 24 {
			fmt.Fprintf(&input, "      - {url: 'http://backend-%03d-%02d:8000', weight: 1, max_inflight: 4}\n", i, j)
		}
	}
	input.WriteString("groups:\n  staff: {models: ['*']}\n")
	big, err := config.Parse([]byte(input.String()), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	document := func(c *config.Config) []byte {
		t.Helper()
		b, e := json.Marshal(settings.Routing{Models: c.Models, Aliases: c.Aliases, Groups: c.Groups, BackendPresets: config.DefaultBackendPresets()})
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	bigJSON := document(big)
	if len(bigJSON) <= settings.MaxDocumentBytes {
		t.Fatal("fixture must exceed document limit", len(bigJSON))
	}
	database := openDatabase(t, newDatabase(t).dsn)
	s := settings.RoutingService{DB: database, File: big}
	actor := audit.Actor{Type: "operator_cli", Name: "settings-size-test"}
	if _, err = s.Import(t.Context(), settings.DomainRouting, actor); !errors.Is(err, settings.ErrDocumentSize) {
		t.Fatal("oversized import has different error", err)
	}
	medium := *big
	medium.Models = map[string]config.Model{}
	for i := range 32 {
		name := fmt.Sprintf("m%03d", i)
		medium.Models[name] = big.Models[name]
	}
	mediumJSON := document(&medium)
	if len(mediumJSON) <= 64<<10 || len(mediumJSON) > settings.MaxDocumentBytes {
		t.Fatal("medium fixture does not cover former CLI gap", len(mediumJSON))
	}
	s.File = &medium
	v, err := s.Import(t.Context(), settings.DomainRouting, actor)
	if err != nil {
		t.Fatal("valid large import refused", err)
	}
	if _, err = s.SaveDraft(t.Context(), settings.DomainRouting, v.Version, bigJSON, nil, actor); !errors.Is(err, settings.ErrDocumentSize) {
		t.Fatal("oversized draft has different error", err)
	}
	v, err = s.SaveDraft(t.Context(), settings.DomainRouting, v.Version, mediumJSON, nil, actor)
	if err != nil || v.Draft == nil || !v.Draft.Validation.Valid {
		t.Fatal("valid large draft refused", err)
	}
}
