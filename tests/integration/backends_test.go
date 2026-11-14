package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/backendlimits"
	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminBackendOnboardingAndOriginBoundKeys(t *testing.T) {
	const key = "synthetic-local-cluster-api-key"
	var inference atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"deepseek-cluster","owned_by":"vllm"}]}`)
		case "/v1/chat/completions":
			var body struct{ Model string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Model != "deepseek-cluster" {
				t.Error("upstream model was not rewritten")
			}
			inference.Add(1)
			_, _ = io.WriteString(w, `{"id":"test","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	var service *settings.RoutingService
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, db *pgxpool.Pool, forwarder *proxy.Handler) {
		cfg.OIDC.SyncIntervalS = 900
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays, RunAt: "03:00"}
		trusted := *cfg
		trusted.Models = map[string]config.Model{"base": cfg.Models["chat-model"], "test-cluster": {Pool: []config.Replica{{URL: backend.URL}}}}
		policy, err := backendpolicy.New(&trusted, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		cipher, err := settings.LoadCipher(func(string) string { return base64.StdEncoding.EncodeToString(make([]byte, 32)) })
		if err != nil {
			t.Fatal(err)
		}
		service = &settings.RoutingService{DB: db, File: cfg, Cipher: cipher, Tokens: policy.Token, Verify: endpoints.VerifyDestinations(policy), Current: func() *config.Config { current, _ := forwarder.Snapshot(); return current }}
		h.Routing, h.RoutingRuntime = service, forwarder
		h.Endpoints = &endpoints.Service{DB: db, Base: cfg, Policy: policy, Runtime: forwarder}
		forwarder.ConfigureDestinations(policy)
		forwarder.BackendLimits = &backendlimits.Manager{DB: db}
	}})
	admin := f.browser(t)
	admin.login(t, "backend-admin", []string{"admins"})
	student := f.browser(t)
	student.login(t, "backend-student", []string{"students"})
	path := "/portal/api/admin/settings/routing"
	call := func(method, path string, body any, want int) []byte {
		t.Helper()
		data, _ := json.Marshal(body)
		origin := f.cfg.Portal.BaseURL
		if method == "GET" {
			data = nil
			origin = ""
		}
		resp, out := admin.call(t, method, path, string(data), origin)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, path, resp.StatusCode, want, out)
		}
		if strings.Contains(string(out), key) {
			t.Fatal("API key exposed in response")
		}
		return out
	}
	view := func(data []byte) settings.RoutingView {
		t.Helper()
		var out routingResponse
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out.Settings
	}
	b := &settings.Backend{Name: "Physics cluster", URL: backend.URL, PrivateNetwork: true, HealthPath: "/v1/models", Groups: []string{"staff"}, Preset: "unlimited"}
	detect := map[string]any{"id": "gpu", "backend": b, "api_key": key}
	encoded, _ := json.Marshal(detect)
	if resp, _ := student.call(t, "POST", "/portal/api/admin/backends/detect", string(encoded), f.cfg.Portal.BaseURL); resp.StatusCode != 403 {
		t.Fatal("non-admin detected backends")
	}
	var catalog endpoints.Catalog
	_ = json.Unmarshal(call("POST", "/portal/api/admin/backends/detect", detect, 200), &catalog)
	if catalog.Provider != "vLLM" || len(catalog.Models) != 1 || catalog.Models[0] != "deepseek-cluster" {
		t.Fatal("new backend detection", catalog)
	}
	v := view(call("POST", path+"/import", struct{}{}, 200))
	doc := *v.Active.Document
	doc.Backends = map[string]*settings.Backend{"gpu": b}
	// A backend can be saved before adding any route to it.
	v = view(call("PUT", path+"/draft", map[string]any{"version": v.Version, "document": doc, "secrets": map[string]string{"gpu": key}}, 200))
	if !v.Draft.Validation.Valid {
		t.Fatal(v.Draft.Validation.Problems)
	}
	doc = *v.Draft.Document
	ref := doc.Backends["gpu"].APIKey.Stored
	if ref == "" {
		t.Fatal("API key was not replaced by a reference")
	}
	var sealed []byte
	if err := f.pool.QueryRow(t.Context(), `SELECT ciphertext FROM managed_secrets WHERE id=$1`, ref).Scan(&sealed); err != nil || strings.Contains(string(sealed), key) {
		t.Fatal("unencrypted API key", err)
	}
	// Discovery can reuse a stored credential, without returning it.
	call("POST", "/portal/api/admin/backends/detect", map[string]any{"id": "gpu", "backend": doc.Backends["gpu"]}, 200)
	doc.Models["cluster-route"] = config.Model{Protocols: []string{"openai"}, UpstreamModel: "deepseek-cluster", TimeoutS: 5, Pool: []config.Replica{{Backend: "gpu", Weight: 1, MaxInflight: 8}}}
	group := doc.Groups["staff"]
	group.Models = append(group.Models, "cluster-route")
	doc.Groups["staff"] = group
	students := doc.Groups["students"]
	students.Models = []string{"*"}
	doc.Groups["students"] = students
	v = view(call("PUT", path+"/draft", map[string]any{"version": v.Version, "document": doc}, 200))
	if !v.Draft.Validation.Valid {
		t.Fatal(v.Draft.Validation.Problems)
	}
	v = view(call("POST", path+"/activate", revisionBody(v.Version, v.Draft.ID), 200))
	resp := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"cluster-route","messages":[]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || inference.Load() != 1 {
		t.Fatal("cluster inference failed", resp.StatusCode)
	}
	response, data := student.call(t, "GET", "/portal/api/models", "", "")
	if response.StatusCode != 200 || strings.Contains(string(data), "cluster-route") {
		t.Fatal("restricted backend exposed in portal catalog", string(data))
	}
	accounts := auth.Admin{Pool: f.pool}
	studentID, err := accounts.AddUser(t.Context(), "student-api", "", []string{"students"})
	if err != nil {
		t.Fatal(err)
	}
	studentKey, err := accounts.CreateKey(t.Context(), studentID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/models"} {
		method := "POST"
		if endpoint == "/v1/models" {
			method = "GET"
		}
		response := f.call(t, studentKey, method, endpoint, "{\"model\":\"cluster-route\",\"messages\":[]}")
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if endpoint == "/v1/models" {
			if strings.Contains(string(data), "cluster-route") {
				t.Fatal("restricted backend exposed by API models")
			}
		} else if response.StatusCode != 403 {
			t.Fatal("restricted inference allowed", response.StatusCode, string(data))
		}
	}
	// Active configuration resolves encrypted credentials again on restart.
	reloaded, _, err := service.Candidate(t.Context(), settings.DomainRouting, v.Active.ID)
	if err != nil || reloaded.Models["cluster-route"].Pool[0].BearerToken != key {
		t.Fatal("stored backend did not reload", err)
	}
	// A backend preset applies to requests from different users, beyond group limits.
	doc = *v.Active.Document
	doc.BackendPresets["one"] = config.BackendRate{Name: "One request", RPM: new(int64(1))}
	doc.Backends["gpu"].Preset = "one"
	v = view(call("PUT", path+"/draft", map[string]any{"version": v.Version, "document": doc}, 200))
	v = view(call("POST", path+"/activate", revisionBody(v.Version, v.Draft.ID), 200))
	anotherID, err := accounts.AddUser(t.Context(), "other-staff", "", []string{"staff"})
	if err != nil {
		t.Fatal(err)
	}
	anotherKey, err := accounts.CreateKey(t.Context(), anotherID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for index, callerKey := range []string{f.key, anotherKey} {
		response := f.call(t, callerKey, "POST", "/v1/chat/completions", "{\"model\":\"cluster-route\",\"messages\":[]}")
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		want := 200
		if index == 1 {
			want = 429
		}
		if response.StatusCode != want {
			t.Fatalf("shared backend RPM: %d want %d", response.StatusCode, want)
		}
	}
	// Rotating only the key still probes the affected route. A rejected key
	// must not silently replace a working route.
	v = view(call("PUT", path+"/draft", map[string]any{"version": v.Version, "document": v.Active.Document, "secrets": map[string]string{"gpu": "incorrect-replacement"}}, 200))
	call("POST", path+"/activate", revisionBody(v.Version, v.Draft.ID), 400)
	v = view(call("GET", path, nil, 200))
	// The same reference cannot be moved to a new backend identity or origin.
	other := *v.Active.Document.Backends["gpu"]
	other.URL = "http://192.168.33.44:8000"
	call("POST", "/portal/api/admin/backends/detect", map[string]any{"id": "gpu", "backend": other}, 400)
	call("POST", "/portal/api/admin/backends/detect", map[string]any{"id": "copied", "backend": v.Active.Document.Backends["gpu"]}, 400)
	doc = *v.Active.Document
	doc.Backends["gpu"] = &other
	v = view(call("PUT", path+"/draft", map[string]any{"version": v.Version, "document": doc}, 200))
	if v.Draft.Validation.Valid {
		t.Fatal("credential reference redirected to a different host")
	}
	call("POST", path+"/activate", revisionBody(v.Version, v.Draft.ID), 400)
}
