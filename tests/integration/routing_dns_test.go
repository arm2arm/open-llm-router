package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/audit"
	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRoutingRecoveryWithUnresolvableReplica(t *testing.T) {
	var service *settings.RoutingService
	var runtime *proxy.Handler
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, p *proxy.Handler) {
		cfg.AdminAPI.Enabled = true
		cfg.OIDC.SyncIntervalS = 900
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays, RunAt: "03:00"}
		policy, err := backendpolicy.New(cfg, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		service = &settings.RoutingService{DB: pool, File: cfg, Tokens: policy.Token, Verify: endpoints.VerifyDestinations(policy), Current: func() *config.Config { c, _ := p.Snapshot(); return c }}
		h.Routing, h.RoutingRuntime = service, p
		runtime = p
	}})
	actor := audit.Actor{Type: "operator_cli", Name: "routing-recovery-test"}
	view, err := service.Import(t.Context(), settings.DomainRouting, actor)
	if err != nil {
		t.Fatal(err)
	}
	healthy := view.Active.ID
	doc := *view.Active.Document
	doc.Models["offline"] = config.Model{Protocols: []string{"openai"}, TimeoutS: 5, Pool: []config.Replica{{URL: "http://olr-deliberately-missing.invalid:8000", Weight: 1, MaxInflight: 1}}}
	encoded, _ := json.Marshal(doc)
	view, err = service.SaveDraft(t.Context(), settings.DomainRouting, view.Version, encoded, nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	view, err = service.Activate(t.Context(), settings.DomainRouting, view.Version, view.Draft.ID, actor, false)
	if err != nil {
		t.Fatal("CLI-style activation needs DNS", err)
	}
	offline := view.Active.ID
	browser := signInLocalAdmin(t, f)
	path := "/portal/api/admin/settings/routing"
	portalCall := func(suffix string, body any, want int) {
		t.Helper()
		b, _ := json.Marshal(body)
		r, data := browser.call(t, "POST", path+suffix, string(b), f.cfg.Portal.BaseURL)
		if r.StatusCode != want {
			t.Fatalf("%s: %d %s", suffix, r.StatusCode, data)
		}
	}
	current := func() settings.RoutingView {
		t.Helper()
		v, e := service.View(t.Context(), settings.DomainRouting)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	revisionBody := func(id int64) map[string]any { return map[string]any{"version": current().Version, "revision": id} }
	portalCall("/apply", map[string]any{}, 200)
	if c, p := runtime.Snapshot(); c.Models["offline"].Pool[0].URL == "" || p.Pools["offline"].Snapshot()[0].Healthy {
		t.Fatal("unresolved replica not visible as unhealthy")
	}
	portalCall("/rollback", revisionBody(healthy), 200)
	portalCall("/rollback", revisionBody(offline), 200)
	portalCall("/rollback", revisionBody(healthy), 200)
	// Agent operations use the same recovery path with bound preview/apply.
	r, data := browser.call(t, "POST", "/portal/api/admin/agent-keys", `{"label":"routing recovery","permissions":["routing:read","routing:write","admin_keys:delegate"],"allowed_networks":["127.0.0.1"],"expires_in_days":1}`, f.cfg.Portal.BaseURL)
	var issued struct{ Key string }
	if r.StatusCode != 201 || json.Unmarshal(data, &issued) != nil || issued.Key == "" {
		t.Fatalf("could not issue agent test credential (HTTP %d)", r.StatusCode)
	}
	agentCall := func(suffix string, body any, want int) []byte {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/api/admin/v1/"+suffix, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+issued.Key)
		req.Header.Set("Content-Type", "application/json")
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("agent %s: %d %s", suffix, resp.StatusCode, out)
		}
		return out
	}
	args := map[string]any{"body": revisionBody(offline)}
	var change struct{ ID string }
	if json.Unmarshal(agentCall("changes/preview", map[string]any{"operation": "rollback_routing", "arguments": args}, 201), &change) != nil || change.ID == "" {
		t.Fatal("missing change")
	}
	agentCall("changes/"+change.ID+"/apply", map[string]any{"arguments": args, "idempotency_key": "routing-dns-recovery"}, 200)
	portalCall("/rollback", revisionBody(healthy), 200)
	// The actual operator binary also accepts the same historical revision.
	dir := t.TempDir()
	binary := filepath.Join(dir, "olr")
	configFile := filepath.Join(dir, "config.yaml")
	if out, e := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", binary, "../../cmd/olr").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, out)
	}
	if e := os.WriteFile(configFile, fmt.Appendf(nil, "models:\n  chat-model:\n    protocols: [openai]\n    timeout_s: 5\n    pool: [{url: %s, weight: 1, max_inflight: 4}]\ngroups:\n  staff: {models: ['*']}\n", f.cfg.Models["chat-model"].Pool[0].URL), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(t.Context(), binary, "admin", "settings", "rollback", "-domain", "routing", "-config", configFile, "-expect-version", fmt.Sprint(current().Version), "-revision", fmt.Sprint(offline))
	cmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+f.pool.Config().ConnString())
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("CLI DNS recovery: %v %s", e, out)
	}
	if current().Active.ID != offline {
		t.Fatal("CLI did not restore requested revision")
	}
	portalCall("/apply", map[string]any{}, 200)
	// The actual CLI accepts a formatted routing document above the old 64 KiB cap.
	largeFile := filepath.Join(dir, "large-routing.json")
	validJSON, _ := json.Marshal(current().Active.Document)
	validJSON = append(validJSON, []byte(strings.Repeat(" ", 80<<10))...)
	if e := os.WriteFile(largeFile, validJSON, 0600); e != nil {
		t.Fatal(e)
	}
	draftCmd := exec.CommandContext(t.Context(), binary, "admin", "settings", "draft", "-domain", "routing", "-config", configFile, "-expect-version", fmt.Sprint(current().Version), "-document", largeFile)
	draftCmd.Env = append(os.Environ(), "OLR_DATABASE_URL="+f.pool.Config().ConnString())
	if out, e := draftCmd.CombinedOutput(); e != nil {
		t.Fatalf("CLI large document: %v %s", e, out)
	}
	// A formerly file-trusted origin becomes forbidden after the operator removes
	// that trust. Recovery must retain destination enforcement on every path.
	strictVerify := service.Verify
	trusted := *f.cfg
	trusted.Models = maps.Clone(f.cfg.Models)
	trusted.Models["retired"] = config.Model{Pool: []config.Replica{{URL: "http://127.0.0.1:9"}}}
	oldPolicy, e := backendpolicy.New(&trusted, func(string) (string, bool) { return "", false })
	if e != nil {
		t.Fatal(e)
	}
	service.Verify = endpoints.VerifyDestinations(oldPolicy)
	v := current()
	blocked := *v.Active.Document
	blocked.Models = maps.Clone(blocked.Models)
	blocked.Models["offline"] = config.Model{Protocols: []string{"openai"}, TimeoutS: 5, Pool: []config.Replica{{URL: "http://127.0.0.1:9", Weight: 1, MaxInflight: 1}}}
	b, _ := json.Marshal(blocked)
	v, e = service.SaveDraft(t.Context(), settings.DomainRouting, v.Version, b, nil, actor)
	if e != nil {
		t.Fatal(e)
	}
	v, e = service.Activate(t.Context(), settings.DomainRouting, v.Version, v.Draft.ID, actor, false)
	if e != nil {
		t.Fatal(e)
	}
	forbidden := v.Active.ID
	service.Verify = strictVerify
	portalCall("/apply", map[string]any{}, 400)
	service.Verify = endpoints.VerifyDestinations(oldPolicy)
	v, e = service.Rollback(t.Context(), settings.DomainRouting, current().Version, healthy, actor)
	if e != nil {
		t.Fatal(e)
	}
	service.Verify = strictVerify
	portalCall("/rollback", revisionBody(forbidden), 400)
	v, e = service.SaveDraft(t.Context(), settings.DomainRouting, current().Version, b, nil, actor)
	if e != nil {
		t.Fatal(e)
	}
	portalCall("/activate", revisionBody(v.Draft.ID), 400)
	if current().Active.ID != healthy {
		t.Fatal("forbidden destination changed active routing")
	}
}
