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
			_, _ = io.WriteString(w, `{"data":[{"id":"deepseek-cluster","owned_by":"vllm","max_model_len":65536}]}`)
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
	b.PublicOrganization, b.PrivacyURL = "Physics Institute", "https://physics.example/privacy"
	detect := map[string]any{"id": "gpu", "backend": b, "api_key": key}
	encoded, _ := json.Marshal(detect)
	if resp, _ := student.call(t, "POST", "/portal/api/admin/backends/detect", string(encoded), f.cfg.Portal.BaseURL); resp.StatusCode != 403 {
		t.Fatal("non-admin detected backends")
	}
	var catalog endpoints.Catalog
	_ = json.Unmarshal(call("POST", "/portal/api/admin/backends/detect", detect, 200), &catalog)
	if catalog.Provider != "vLLM" || len(catalog.Models) != 1 || catalog.Models[0] != "deepseek-cluster" || catalog.ContextLengths["deepseek-cluster"] != 65536 {
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
	// A configured context length and type are stored in the revision.
	pinned := 4096
	doc.Models["cluster-embed"] = config.Model{Protocols: []string{"openai"}, UpstreamModel: "deepseek-cluster", TimeoutS: 5, ContextLength: &pinned, Type: config.ModelTypeEmbedding, Pool: []config.Replica{{Backend: "gpu", Weight: 1, MaxInflight: 8}}}
	doc.Aliases["cluster-alias"] = "cluster-route"
	group := doc.Groups["staff"]
	group.Models = append(group.Models, "cluster-route", "cluster-embed")
	doc.Groups["staff"] = group
	students := doc.Groups["students"]
	students.Models = []string{"*"}
	doc.Groups["students"] = students
	v = view(call("PUT", path+"/draft", map[string]any{"version": v.Version, "document": doc}, 200))
	if !v.Draft.Validation.Valid {
		t.Fatal(v.Draft.Validation.Problems)
	}
	v = view(call("POST", path+"/activate", revisionBody(v.Version, v.Draft.ID), 200))
	if stored := v.Active.Document.Models["cluster-embed"]; stored.ContextLength == nil || *stored.ContextLength != pinned || stored.Type != config.ModelTypeEmbedding || v.Active.Document.Models["cluster-route"].ContextLength != nil {
		t.Fatalf("model metadata did not survive the revision: %+v", stored)
	}
	// Public disclosure survives the stored revision and uses its active model
	// and alias mapping in both authenticated catalogues. Both report the
	// context length detected from the backend's catalog, or configured.
	for _, endpoint := range []string{"/portal/api/models", "/v1/models"} {
		var data []byte
		if endpoint == "/portal/api/models" {
			data = call("GET", endpoint, nil, 200)
		} else {
			response := f.call(t, f.key, "GET", endpoint, "")
			data, _ = io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode, string(data))
			}
		}
		var catalogue []struct {
			ID         string `json:"id"`
			Recipients []struct {
				Organization string `json:"organization"`
				PrivacyURL   string `json:"privacy_url"`
			} `json:"recipient_details"`
			Complete      bool     `json:"recipients_complete"`
			ContextLength int      `json:"context_length"`
			Type          string   `json:"type"`
			Protocols     []string `json:"protocols"`
		}
		// Other top-level fields are strings; decode only the model array.
		var envelope map[string]json.RawMessage
		if json.Unmarshal(data, &envelope) != nil {
			t.Fatal(string(data))
		}
		field := "data"
		if endpoint == "/portal/api/models" {
			field = "models"
		}
		if json.Unmarshal(envelope[field], &catalogue) != nil {
			t.Fatal(string(data))
		}
		found := 0
		for _, model := range catalogue {
			if model.ID != "cluster-route" && model.ID != "cluster-alias" && model.ID != "cluster-embed" {
				continue
			}
			found++
			if !model.Complete || len(model.Recipients) != 1 || model.Recipients[0].Organization != b.PublicOrganization || model.Recipients[0].PrivacyURL != b.PrivacyURL {
				t.Fatal("recipient metadata did not activate", string(data))
			}
			length, kind := 65536, config.ModelTypeChat
			if model.ID == "cluster-embed" {
				length, kind = pinned, config.ModelTypeEmbedding
			}
			if model.ContextLength != length || model.Type != kind || len(model.Protocols) != 1 || model.Protocols[0] != "openai" {
				t.Fatalf("%s %s: context_length %d type %q protocols %v", endpoint, model.ID, model.ContextLength, model.Type, model.Protocols)
			}
		}
		if found != 3 || strings.Contains(string(data), backend.URL) || strings.Contains(string(data), key) || strings.Contains(string(data), "Physics cluster") {
			t.Fatal("catalogue missing public recipients or exposing private configuration", string(data))
		}
	}
	resp := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"cluster-route","messages":[]}`)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || inference.Load() != 1 {
		t.Fatal("cluster inference failed", resp.StatusCode)
	}
	// An administrator's connection test uses the active backend credential,
	// model rewrite and accounting path; ordinary sessions and cross-site
	// requests cannot invoke it.
	testPath := "/portal/api/admin/backends/test"
	testBody := map[string]any{"model": "cluster-route", "backend": "gpu", "api": "chat"}
	adminCall(t, student, "POST", testPath, testBody, 403)
	if response, _ := admin.call(t, "POST", testPath, string(mustJSON(t, testBody)), "https://evil.example"); response.StatusCode != 403 {
		t.Fatal("cross-site model test accepted")
	}
	if response, _ := admin.call(t, "POST", testPath, `{"model":"`+strings.Repeat("x", 4097)+`"}`, f.cfg.Portal.BaseURL); response.StatusCode != 400 {
		t.Fatal("unbounded model test body", response.StatusCode)
	}
	var result proxy.ConnectionResult
	if json.Unmarshal(call("POST", testPath, testBody, 200), &result) != nil || !result.OK || result.HTTPStatus != 200 || result.OutputTokens != 1 || inference.Load() != 2 {
		t.Fatal("functional connection test failed", result, inference.Load())
	}
	waitFor(t, func() bool {
		var count int
		err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM usage u JOIN users p ON p.id=u.user_id WHERE u.request_id=$1 AND p.username='backend-admin' AND u.output_tokens=1 AND u.key_id IS NULL`, result.RequestID).Scan(&count)
		return err == nil && count == 1
	})
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
	if err != nil || reloaded.Models["cluster-route"].Pool[0].BearerToken != key || reloaded.Models["cluster-route"].Pool[0].PublicOrganization != b.PublicOrganization || reloaded.Models["cluster-route"].Pool[0].PrivacyURL != b.PrivacyURL {
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
	before := inference.Load()
	if json.Unmarshal(call("POST", testPath, testBody, 200), &result) != nil || result.OK || result.HTTPStatus != 429 || inference.Load() != before {
		t.Fatal("connection test bypassed shared backend limits", result)
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
