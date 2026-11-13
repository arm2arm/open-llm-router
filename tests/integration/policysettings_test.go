package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/limits"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManagedPoliciesPreviewEnforcementAndRollback(t *testing.T) {
	var service *settings.RoutingService
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, db *pgxpool.Pool, p *proxy.Handler) {
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.OIDC.SyncIntervalS = 900
		cfg.Retention = config.Retention{UsageDays: 90, UsageDailyDays: 400, RunAt: "03:00"}
		service = &settings.RoutingService{DB: db, File: cfg, Current: func() *config.Config { current, _ := p.Snapshot(); return current }}
		h.Routing, h.RoutingRuntime = service, p
	}})
	admin := f.browser(t)
	admin.login(t, "policy-admin", []string{"admins"})
	reader := f.browser(t)
	reader.login(t, "policy-reader", []string{"students"})
	path := "/portal/api/admin/settings/routing"
	call := func(method, suffix string, body any, want int) []byte {
		t.Helper()
		encoded, _ := json.Marshal(body)
		resp, data := admin.call(t, method, path+suffix, string(encoded), f.cfg.Portal.BaseURL)
		if resp.StatusCode != want {
			t.Fatalf("%s: got %d want %d: %s", suffix, resp.StatusCode, want, data)
		}
		return data
	}
	var imported routingResponse
	_ = json.Unmarshal(call("POST", "/import", struct{}{}, 200), &imported)
	doc := *imported.Settings.Active.Document
	// Both groups have finite interactive and batch defaults. Wildcard grants
	// remain canonical and a stored override is authoritative over either group.
	doc.Groups["staff"] = config.Group{Models: []string{"chat-model"}, RPM: limitPtr(3), MaxConcurrent: limitPtr(2), MaxConcurrentBatch: limitPtr(4), TokensPerDay: limitPtr(int64(100)), StrictQuota: true, Priority: 5}
	doc.Groups["students"] = config.Group{Models: []string{"*"}, RPM: limitPtr(7), MaxConcurrent: limitPtr(3), TokensPerDay: limitPtr(int64(200)), StrictQuota: true, Priority: -2}
	a := auth.Admin{Pool: f.pool}
	override := int64(2)
	if err := a.SetOverride(t.Context(), f.owner, "rpm", &override, "test preview", "operator"); err != nil {
		t.Fatal(err)
	}
	type preview struct {
		Models    []string
		Admin     bool
		Priority  int
		Limits    limits.EffectiveLimits
		Overrides map[string]*int64
		Affected  []struct{ ID string } `json:"affected_users"`
	}
	var p preview
	_ = json.Unmarshal(call("POST", "/preview", map[string]any{"document": doc, "user_id": f.owner, "memberships": []string{"staff", "students", "unknown"}}, 200), &p)
	if p.Limits.RPM == nil || *p.Limits.RPM != 2 || p.Limits.MaxConcurrentBatch == nil || *p.Limits.MaxConcurrentBatch != 4 || p.Priority != -2 || !p.Limits.StrictQuota || len(p.Models) != 1 || len(p.Affected) == 0 {
		t.Fatalf("preview does not resolve policy/override: %+v", p)
	}
	// One omitted value makes a merged field unlimited; strict quota is AND.
	student := doc.Groups["students"]
	student.TokensPerDay = nil
	student.StrictQuota = false
	doc.Groups["students"] = student
	_ = json.Unmarshal(call("POST", "/preview", map[string]any{"document": doc, "memberships": []string{"staff", "students"}}, 200), &p)
	if p.Limits.TokensPerDay != nil || p.Limits.StrictQuota {
		t.Fatal("unlimited/strict merge", p)
	}
	// Strip students' access to the existing canonical model by replacing their
	// grant with a different model and removing administrators' group authority.
	restricted := doc.Models["chat-model"]
	restricted.UpstreamModel = "chat-model"
	doc.Models["restricted"] = restricted
	student.Models = []string{"restricted"}
	doc.Groups["students"] = student
	doc.Groups["admins"] = config.Group{Models: []string{"*"}}
	// Keep an API-level admin available using the protected local account.
	root, _, err := a.BootstrapLocalAdmin(t.Context(), []byte(initialAdminPassword))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ChangeLocalPassword(t.Context(), root.ID, []byte(initialAdminPassword), []byte(rotatedAdminPassword)); err != nil {
		t.Fatal(err)
	}
	admin.localLogin(t, "admin", rotatedAdminPassword, 200)
	if err = a.SetOverride(t.Context(), root.ID, "rpm", &override, "restrict root", "operator"); !errors.Is(err, auth.ErrProtectedAdmin) {
		t.Fatal("protected override", err)
	}
	var saved routingResponse
	_ = json.Unmarshal(call("PUT", "/draft", map[string]any{"version": imported.Settings.Version, "document": doc}, 200), &saved)
	if !saved.Settings.Draft.Validation.Valid {
		t.Fatal(saved.Settings.Draft.Validation.Problems)
	}
	call("POST", "/activate", revisionBody(saved.Settings.Version, saved.Settings.Draft.ID), 200)
	if resp, _ := reader.call(t, "POST", path+"/preview", `{"memberships":[]}`, f.cfg.Portal.BaseURL); resp.StatusCode != 403 {
		t.Fatal("ordinary user preview", resp.StatusCode)
	}
	if resp, _ := reader.call(t, "PUT", path+"/draft", `{}`, f.cfg.Portal.BaseURL); resp.StatusCode != 403 {
		t.Fatal("ordinary user edit", resp.StatusCode)
	}
	// Existing API keys immediately see the managed defaults and stored override.
	resp := f.call(t, f.key, "GET", "/v1/me/usage", "")
	var usage struct{ Limits limits.EffectiveLimits }
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(data, &usage)
	if usage.Limits.RPM == nil || *usage.Limits.RPM != 2 {
		t.Fatal("runtime override", string(data))
	}
	// A second local user can only see/use their canonical grant and its pool.
	id, err := a.AddUser(t.Context(), "restricted-user", "", []string{"students"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := a.CreateKey(t.Context(), id, "restricted", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]int{"restricted": 200, "chat-model": 403} {
		if got := f.call(t, key, "POST", "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[]}`, model)).StatusCode; got != want {
			t.Fatalf("%s: %d want %d", model, got, want)
		}
	}
	// Referenced groups cannot vanish; invalid canonical grants cannot activate.
	delete(doc.Groups, "students")
	_ = json.Unmarshal(call("PUT", "/draft", map[string]any{"version": saved.Settings.Version + 1, "document": doc}, 200), &saved)
	if saved.Settings.Draft.Validation.Valid || !strings.Contains(strings.Join(saved.Settings.Draft.Validation.Problems, " "), "still referenced") {
		t.Fatal("group deletion", saved)
	}
	call("POST", "/activate", revisionBody(saved.Settings.Version, saved.Settings.Draft.ID), 400)
	call("POST", "/rollback", revisionBody(saved.Settings.Version, imported.Settings.Active.ID), 200)
	if got := f.call(t, key, "POST", "/v1/chat/completions", `{"model":"chat-model","messages":[]}`).StatusCode; got != 200 {
		t.Fatal("rollback grant", got)
	}
	// Startup reads the full policy/routing revision rather than changed YAML.
	snapshot, err := service.Apply(t.Context(), f.cfg, slog.New(slog.DiscardHandler))
	if err != nil || snapshot.Config.Groups["students"].Models[0] != "chat-model" {
		t.Fatal("restart", err)
	}
}
