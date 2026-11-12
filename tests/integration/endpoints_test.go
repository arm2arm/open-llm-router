package integration_test

import (
	"bufio"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/policy"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
)

const endpointToken = "private-synthetic-backend-token"

type endpointFixture struct {
	*portalFixture
	service     *endpoints.Service
	forwarder   *proxy.Handler
	replacement config.Model
}

func startEndpointPortal(t *testing.T, initial http.Handler) *endpointFixture {
	t.Helper()
	p := newIdentityProvider(t)
	f := &endpointFixture{portalFixture: &portalFixture{provider: p}}
	real := mockllm.New(mockllm.Options{Models: []string{"backend-real"}, Tokens: 3})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+endpointToken || r.Header.Get("X-Api-Key") != "" {
			t.Error("backend credential isolation failed")
			w.WriteHeader(401)
			return
		}
		real.ServeHTTP(w, r)
	}))
	t.Cleanup(backend.Close)
	f.usageRouter = startUsageRouter(t, initial, usageOptions{configure: func(cfg *config.Config, forwarder *proxy.Handler, s *server.Server, db *pgxpool.Pool) {
		cfg.OIDC = &config.OIDC{Issuer: p.issuer, PortalClientID: "olr-portal", GroupsClaim: "groups", UsernameClaim: "preferred_username", SyncIntervalS: 900}
		cfg.OIDCClientSecret = "test-secret"
		cfg.Portal = config.Portal{BaseURL: "http://portal.example.test", CookieSecure: false}
		cfg.Retention = config.Retention{UsageDays: 90, UsageDailyDays: 400, RunAt: "03:00"}
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Groups["admins"] = config.Group{Models: []string{"*"}, Admin: true}
		staff := cfg.Groups["staff"]
		staff.MaxConcurrent = limitPtr(1)
		cfg.Groups["staff"] = staff
		f.replacement = config.Model{Protocols: []string{"openai"}, UpstreamModel: "backend-real", TimeoutS: 5, IdleTimeoutS: 3, Pool: []config.Replica{{URL: backend.URL, HealthPath: "/health", Weight: 1, MaxInflight: 2, BearerTokenEnv: "ENDPOINT_GPU_TOKEN", BearerToken: endpointToken}}}
		// This second file-defined model explicitly trusts the loopback test
		// server; newly supplied loopback/control destinations remain forbidden.
		cfg.Models["secondary"] = f.replacement
		policy, err := backendpolicy.New(cfg, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		forwarder.ConfigureDestinations(policy)
		publish, _, err := forwarder.Prepare(t.Context(), cfg, "chat-model")
		if err != nil {
			t.Fatal(err)
		}
		publish()
		f.service = &endpoints.Service{DB: db, Base: cfg, Policy: policy, Runtime: forwarder}
		h, err := portal.New(t.Context(), cfg, db, forwarder.Limits)
		if err != nil {
			t.Fatal(err)
		}
		h.Pools = forwarder.Pools
		h.Endpoints = f.service
		t.Cleanup(h.Close)
		s.Handle("/portal", h)
		s.Handle("/portal/", h)
		f.cfg = cfg
		f.forwarder = forwarder
	}})
	waitFor(t, func() bool {
		_, body := f.browser(t).call(t, "GET", "/portal/api/auth", "", "")
		var methods struct{ OIDC struct{ Available bool } }
		return json.Unmarshal(body, &methods) == nil && methods.OIDC.Available
	})
	return f
}

func endpointEntries(t *testing.T, operator *portalBrowser) []endpoints.Entry {
	t.Helper()
	data := adminCall(t, operator, "GET", "/portal/api/admin/endpoints", nil, 200)
	if strings.Contains(string(data), endpointToken) || strings.Contains(string(data), "bearer_token\"") {
		t.Fatal("endpoint settings exposed credential material")
	}
	var response struct{ Models []endpoints.Entry }
	if json.Unmarshal(data, &response) != nil {
		t.Fatal("invalid settings response")
	}
	return response.Models
}
func endpointEntry(t *testing.T, operator *portalBrowser, name string) endpoints.Entry {
	t.Helper()
	for _, entry := range endpointEntries(t, operator) {
		if entry.Model == name {
			return entry
		}
	}
	t.Fatal("model missing from endpoint settings")
	return endpoints.Entry{}
}
func saveEndpoints(t *testing.T, operator *portalBrowser, model string, revision int64, configuration config.Model, want int) {
	t.Helper()
	adminCall(t, operator, "PUT", "/portal/api/admin/endpoints", map[string]any{"model": model, "revision": revision, "configuration": configuration}, want)
}

func TestEndpointConfigurationLifecyclePersistenceAndGuards(t *testing.T) {
	var failHealth atomic.Bool
	mock := mockllm.New(mockllm.Options{Tokens: 2})
	f := startEndpointPortal(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && failHealth.Load() {
			w.WriteHeader(503)
			return
		}
		mock.ServeHTTP(w, r)
	}))
	operator, ordinary := f.browser(t), f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	ordinary.login(t, "reader", []string{"staff"})
	initial := endpointEntry(t, operator, "chat-model")
	if initial.Source != "file" || initial.Revision != 0 {
		t.Fatal("initial source is not file configuration")
	}
	// Activation requires a fresh successful probe, even when the existing
	// pool's two-failure health threshold still considers it healthy.
	failHealth.Store(true)
	saveEndpoints(t, operator, "chat-model", 0, initial.Configuration, 400)
	failHealth.Store(false)
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/portal/api/admin/endpoints", nil},
		{"PUT", "/portal/api/admin/endpoints", map[string]any{"model": "chat-model", "revision": 0, "configuration": f.replacement}},
		{"POST", "/portal/api/admin/endpoints/test", map[string]any{"model": "chat-model", "configuration": f.replacement}},
		{"POST", "/portal/api/admin/endpoints/models", map[string]any{"model": "chat-model", "replicas": []endpoints.ModelSource{{URL: f.replacement.Pool[0].URL, BearerTokenEnv: "ENDPOINT_GPU_TOKEN"}}}},
		{"POST", "/portal/api/admin/endpoints/restore", map[string]any{"model": "chat-model", "revision": 0, "target_revision": 0}},
	} {
		adminCall(t, ordinary, request.method, request.path, request.body, 403)
		if request.method != "GET" {
			r, _ := operator.call(t, request.method, request.path, string(mustJSON(t, request.body)), "https://evil.example")
			if r.StatusCode != 403 {
				t.Fatal("cross-origin endpoint mutation accepted")
			}
		}
	}
	catalogData := adminCall(t, operator, "POST", "/portal/api/admin/endpoints/models", map[string]any{"model": "chat-model", "replicas": []endpoints.ModelSource{{URL: f.replacement.Pool[0].URL, BearerTokenEnv: "ENDPOINT_GPU_TOKEN"}}}, 200)
	var catalog endpoints.Discovery
	if json.Unmarshal(catalogData, &catalog) != nil || !catalog.Complete || len(catalog.Models) != 1 || catalog.Models[0] != "backend-real" || strings.Contains(string(catalogData), endpointToken) {
		t.Fatal("model detection failed or leaked secret")
	}
	adminCall(t, operator, "POST", "/portal/api/admin/endpoints/models", map[string]any{"model": "chat-model", "replicas": []map[string]string{{"url": f.replacement.Pool[0].URL, "bearer_token": "forbidden"}}}, 400)
	data := adminCall(t, operator, "POST", "/portal/api/admin/endpoints/test", map[string]any{"model": "chat-model", "configuration": f.replacement}, 200)
	var result struct{ OK bool }
	if json.Unmarshal(data, &result) != nil || !result.OK {
		t.Fatal("valid connection test failed")
	}
	if entry := endpointEntry(t, operator, "chat-model"); entry.Revision != 0 {
		t.Fatal("connection test changed active settings")
	}
	saveEndpoints(t, operator, "chat-model", 0, f.replacement, 200)
	first := endpointEntry(t, operator, "chat-model")
	if first.Revision == 0 || first.Source != "database" || first.Configuration.UpstreamModel != "backend-real" {
		t.Fatal("saved settings not activated")
	}
	if response := f.request(t, "/v1/chat/completions", ""); !strings.Contains(string(response), `"model":"backend-real"`) {
		t.Fatal("upstream model rewrite not active")
	}
	loaded, err := endpoints.Load(t.Context(), f.pool, f.cfg, f.service.Policy)
	if err != nil || loaded.Models["chat-model"].UpstreamModel != "backend-real" {
		t.Fatal("persisted settings not loaded on restart", err)
	}
	if f.cfg.Models["chat-model"].UpstreamModel != "" {
		t.Fatal("file configuration was mutated")
	}
	saveEndpoints(t, operator, "chat-model", 0, initial.Configuration, 409)
	saveEndpoints(t, operator, "missing-model", 0, f.replacement, 404)
	bad := f.replacement
	bad.Pool = append([]config.Replica(nil), bad.Pool...)
	for _, change := range []func(){
		func() { bad.Pool[0].URL = "http://user:private-url-password@server" },
		func() { bad.Pool[0].URL = "http://169.254.169.254" },
		func() { bad.Pool[0].URL = "http://127.0.0.1:8080" },
		func() { bad.Pool[0].URL = "http://192.168.2.10:11434" },
		func() { bad.Pool[0].BearerTokenEnv = "OLR_DATABASE_URL" },
		func() { bad.Pool = append(bad.Pool, bad.Pool[0]) },
		func() { bad.Protocols = []string{"unknown"} },
		func() { bad.Pool = nil },
	} {
		bad = f.replacement
		bad.Pool = append([]config.Replica(nil), bad.Pool...)
		change()
		body := map[string]any{"model": "chat-model", "revision": first.Revision, "configuration": bad}
		data := adminCall(t, operator, "PUT", "/portal/api/admin/endpoints", body, 400)
		if strings.Contains(string(data), "private-url-password") {
			t.Fatal("URL validation disclosed credentials")
		}
	}
	bad = f.replacement
	bad.Pool = append([]config.Replica(nil), bad.Pool...)
	bad.Pool[0].HealthPath = "/not-a-health-path"
	saveEndpoints(t, operator, "chat-model", first.Revision, bad, 400)
	if entry := endpointEntry(t, operator, "chat-model"); entry.Revision != first.Revision || len(entry.History) != 1 {
		t.Fatal("failed validation changed active revision/history")
	}
	f.request(t, "/v1/chat/completions", "")
	saveEndpoints(t, operator, "chat-model", first.Revision, initial.Configuration, 200)
	second := endpointEntry(t, operator, "chat-model")
	adminCall(t, operator, "POST", "/portal/api/admin/endpoints/restore", map[string]any{"model": "chat-model", "revision": second.Revision, "target_revision": first.Revision}, 200)
	third := endpointEntry(t, operator, "chat-model")
	if third.Revision == first.Revision || third.Configuration.UpstreamModel != "backend-real" {
		t.Fatal("rollback did not create a restored revision")
	}
	adminCall(t, operator, "POST", "/portal/api/admin/endpoints/restore", map[string]any{"model": "secondary", "revision": 0, "target_revision": first.Revision}, 404)
	adminCall(t, operator, "POST", "/portal/api/admin/endpoints/restore", map[string]any{"model": "chat-model", "revision": third.Revision, "target_revision": 0}, 200)
	reset := endpointEntry(t, operator, "chat-model")
	if reset.Revision == 0 || reset.Source != "file" || reset.Configuration.UpstreamModel != "" || reset.Configuration.Pool[0].URL != initial.Configuration.Pool[0].URL {
		t.Fatal("restore to file failed or reused revision 0")
	}
	saveEndpoints(t, operator, "chat-model", 0, f.replacement, 409)
	var stored string
	if err := f.pool.QueryRow(t.Context(), `SELECT string_agg(configuration::text,'') FROM endpoint_revisions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, endpointToken) || !strings.Contains(stored, "ENDPOINT_GPU_TOKEN") || strings.Contains(f.logs.String(), endpointToken) {
		t.Fatal("persistent revision/log credential disclosure")
	}
	adminCall(t, operator, "POST", "/portal/api/admin/endpoints/restore", map[string]any{"model": "secondary", "revision": 0, "target_revision": 0}, 200)
	reduced := *f.cfg
	reduced.Models = maps.Clone(f.cfg.Models)
	delete(reduced.Models, "secondary")
	if _, err := endpoints.Load(t.Context(), f.pool, &reduced, f.service.Policy); err != nil {
		t.Fatal("a model restored to file ownership could not be removed from YAML", err)
	}
}

func TestEndpointActivationPreservesCommittedStreamAndUserAdmission(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	initial := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(200)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"old-stream-marker\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	})
	f := startEndpointPortal(t, initial)
	operator := f.browser(t)
	operator.login(t, "operator", []string{"admins"})
	req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[],"stream":true,"stream_options":{"include_usage":true}}`))
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	response, err := f.router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(first, "old-stream-marker") {
		t.Fatal("stream did not commit its first event")
	}
	manager := f.forwarder.Limits
	saveEndpoints(t, operator, "chat-model", 0, f.replacement, 200)
	if f.forwarder.Limits != manager {
		t.Fatal("activation replaced the admission manager")
	}
	second, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"chat-model","messages":[]}`))
	second.Header.Set("Authorization", "Bearer "+f.key)
	denied, err := f.router.Client().Do(second)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != 429 {
		t.Fatal("activation reset active user concurrency", denied.StatusCode)
	}
	releaseOnce.Do(func() { close(release) })
	remaining, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(remaining), "[DONE]") {
		t.Fatal("activation interrupted the committed stream", err)
	}
	waitFor(t, func() bool {
		snapshot, err := manager.Snapshot(t.Context(), f.owner, policy.Policy{})
		return err == nil && snapshot.InFlight == 0
	})
	f.request(t, "/v1/chat/completions", "")
	waitFor(t, func() bool {
		var count int
		return f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage WHERE user_id=$1 AND model='chat-model' AND status LIKE '2%'`, f.owner).Scan(&count) == nil && count == 2
	})
}
