package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/backendpolicy"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/endpoints"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/portal"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
)

type routingResponse struct {
	Settings settings.RoutingView `json:"settings"`
	Runtime  struct{ Revision int64 }
}

// Admin creates, aliases, checks, activates and rolls back logical models as
// complete routing revisions, while streams and accounting continue.
func TestAdminRoutingRevisions(t *testing.T) {
	const backendToken = "private-backend-token-value"
	second := mockllm.New(mockllm.Options{Tokens: 20, TokenDelay: 25 * time.Millisecond})
	secondServer := httptest.NewServer(second)
	defer secondServer.Close()
	environment := map[string]string{"OLR_BACKEND_TOKEN_ENVS": "GPU_TOKEN", "GPU_TOKEN": backendToken}
	f := startPortal(t, usageOptions{portal: func(h *portal.Handler, cfg *config.Config, pool *pgxpool.Pool, forwarder *proxy.Handler) {
		cfg.OIDC.SyncIntervalS = 900
		cfg.Forwarders.UnknownUser = config.UnknownUserReject
		cfg.Retention = config.Retention{UsageDays: config.DefaultUsageDays, UsageDailyDays: config.DefaultUsageDailyDays, RunAt: "03:00"}
		// File-defined origins are trusted by the destination policy; the
		// second backend is "in the file" for the policy only.
		trusted := *cfg
		trusted.Models = map[string]config.Model{"chat-model": cfg.Models["chat-model"], "spare": {Pool: []config.Replica{{URL: secondServer.URL}}}}
		policy, err := backendpolicy.New(&trusted, func(name string) (string, bool) { value, ok := environment[name]; return value, ok })
		if err != nil {
			t.Fatal(err)
		}
		h.Routing = &settings.RoutingService{DB: pool, File: cfg, Tokens: policy.Token, Verify: endpoints.VerifyDestinations(policy),
			Current: func() *config.Config { current, _ := forwarder.Snapshot(); return current }}
		h.RoutingRuntime = forwarder
		h.Endpoints = &endpoints.Service{DB: pool, Base: cfg, Policy: policy, Runtime: forwarder}
	}})
	base := f.cfg.Portal.BaseURL
	path := "/portal/api/admin/settings/routing"
	admin := f.browser(t)
	admin.login(t, "routing-admin", []string{"admins"})
	student := f.browser(t)
	student.login(t, "routing-student", []string{"students"})
	if resp, _ := student.call(t, "GET", path, "", ""); resp.StatusCode != 403 {
		t.Fatal("a non-admin read routing settings", resp.StatusCode)
	}
	call := func(method, suffix string, body any, want int) routingResponse {
		t.Helper()
		encoded, origin := "", ""
		if body != nil {
			data, _ := json.Marshal(body)
			encoded, origin = string(data), base
		}
		resp, data := admin.call(t, method, path+suffix, encoded, origin)
		if resp.StatusCode != want {
			t.Fatalf("%s %s: %d want %d: %s", method, suffix, resp.StatusCode, want, data)
		}
		if strings.Contains(string(data), backendToken) {
			t.Fatal("a routing response exposed a backend credential")
		}
		var view routingResponse
		_ = json.Unmarshal(data, &view)
		return view
	}
	complete := func(model string, want int) {
		t.Helper()
		resp := f.call(t, f.key, "POST", "/v1/chat/completions", `{"model":"`+model+`","messages":[]}`)
		if resp.StatusCode != want {
			t.Fatalf("completion for %s: %d want %d", model, resp.StatusCode, want)
		}
	}

	view := call("GET", "", nil, 200)
	if view.Settings.Managed || view.Runtime.Revision != 0 {
		t.Fatal("initial routing view", view)
	}
	view = call("POST", "/import", struct{}{}, 200)
	imported := view.Settings.Active.ID
	document := func(mutate func(*settings.Routing)) settings.Routing {
		doc := *call("GET", "", nil, 200).Settings.Active.Document
		copied := settings.Routing{Models: map[string]config.Model{}, Aliases: map[string]string{}}
		for name, model := range doc.Models {
			copied.Models[name] = model
		}
		for alias, target := range doc.Aliases {
			copied.Aliases[alias] = target
		}
		mutate(&copied)
		return copied
	}
	save := func(doc settings.Routing) settings.RoutingView {
		t.Helper()
		version := call("GET", "", nil, 200).Settings.Version
		return call("PUT", "/draft", map[string]any{"version": version, "document": doc}, 200).Settings
	}
	newModel := config.Model{Protocols: []string{"openai"}, UpstreamModel: "chat-model", TimeoutS: 5, Pool: []config.Replica{{URL: secondServer.URL, Weight: 1, MaxInflight: 8, BearerTokenEnv: "GPU_TOKEN"}}}

	// Invalid drafts are recorded but never replace the active routing.
	for name, mutate := range map[string]func(*settings.Routing){
		"metadata destination": func(r *settings.Routing) {
			r.Models["second-model"] = config.Model{Protocols: []string{"openai"}, TimeoutS: 5, Pool: []config.Replica{{URL: "http://169.254.169.254", Weight: 1, MaxInflight: 1}}}
		},
		"model still granted by a group": func(r *settings.Routing) {
			r.Models["second-model"] = newModel
			delete(r.Models, "chat-model")
		},
		"unknown credential reference": func(r *settings.Routing) {
			model := newModel
			model.Pool = []config.Replica{{URL: secondServer.URL, Weight: 1, MaxInflight: 1, BearerTokenEnv: "OLR_DATABASE_URL"}}
			r.Models["second-model"] = model
		},
	} {
		draft := save(document(mutate)).Draft
		if draft.Validation.Valid {
			t.Fatal(name, "draft is valid")
		}
		if name == "metadata destination" {
			// Checks report a refused destination without contacting it.
			_, data := admin.call(t, "POST", path+"/checks", `{"revision":`+jsonNumber(draft.ID)+`}`, base)
			if !strings.Contains(string(data), `"name":"destination","status":"failed"`) || strings.Contains(string(data), "passed") {
				t.Fatal("destination check", string(data))
			}
		}
		version := call("GET", "", nil, 200).Settings.Version
		call("POST", "/activate", revisionBody(version, draft.ID), 400)
		if current := call("GET", "", nil, 200); current.Settings.Active.ID != imported {
			t.Fatal(name, "changed the active routing")
		}
	}
	// A document carrying a resolved credential is refused outright.
	version := call("GET", "", nil, 200).Settings.Version
	if resp, _ := admin.call(t, "PUT", path+"/draft", `{"version":`+jsonNumber(version)+`,"document":{"models":{"m":{"pool":[{"url":"http://x","BearerToken":"t"}]}}}}`, base); resp.StatusCode != 400 {
		t.Fatal("a resolved token field was accepted", resp.StatusCode)
	}

	// Create a model with a credential reference and an alias; activate live.
	draft := save(document(func(r *settings.Routing) {
		r.Models["second-model"] = newModel
		r.Aliases["fast"] = "second-model"
	})).Draft
	if !draft.Validation.Valid {
		t.Fatal("valid draft", draft.Validation.Problems)
	}
	complete("second-model", 404)
	var checked struct {
		Results []struct{ Name, Status string }
	}
	_, data := admin.call(t, "POST", path+"/checks", `{"revision":`+jsonNumber(draft.ID)+`}`, base)
	if json.Unmarshal(data, &checked) != nil || len(checked.Results) != 1 || checked.Results[0].Status != "passed" || checked.Results[0].Name != "models.second-model.pool[0]" {
		t.Fatal("endpoint checks", string(data))
	}
	view = call("POST", "/activate", revisionBody(call("GET", "", nil, 200).Settings.Version, draft.ID), 200)
	if view.Runtime.Revision != draft.ID {
		t.Fatal("runtime revision after activation", view.Runtime.Revision)
	}
	complete("second-model", 200)
	complete("fast", 200)
	if second.Stats().Requests != 2 {
		t.Fatal("the new model's backend did not serve", second.Stats().Requests)
	}
	// Per-model endpoint editing is closed while routing is managed.
	if resp, _ := admin.call(t, "PUT", "/portal/api/admin/endpoints", `{"model":"chat-model","revision":0,"configuration":{}}`, base); resp.StatusCode != 409 {
		t.Fatal("the endpoint editor still saves", resp.StatusCode)
	}

	// A stream started before another activation finishes, and is accounted.
	written := f.writer.Counters().Written
	streamDone := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest("POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"second-model","messages":[],"stream":true}`))
		req.Header.Set("Authorization", "Bearer "+f.key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.router.Client().Do(req)
		if err != nil {
			streamDone <- err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		streamDone <- string(body)
	}()
	waitFor(t, func() bool { return second.Stats().Requests == 3 })
	reweighted := save(document(func(r *settings.Routing) {
		model := r.Models["second-model"]
		model.Pool = []config.Replica{{URL: secondServer.URL, Weight: 2, MaxInflight: 8, BearerTokenEnv: "GPU_TOKEN"}}
		r.Models["second-model"] = model
	})).Draft
	call("POST", "/activate", revisionBody(call("GET", "", nil, 200).Settings.Version, reweighted.ID), 200)
	if body := <-streamDone; !strings.Contains(body, "[DONE]") {
		t.Fatal("a stream did not survive activation", body)
	}
	waitFor(t, func() bool { return f.writer.Counters().Written == written+1 })

	// Concurrent edits conflict; rollback restores the imported routing.
	call("PUT", "/draft", map[string]any{"version": draft.ID, "document": document(func(*settings.Routing) {})}, 409)
	view = call("POST", "/rollback", revisionBody(call("GET", "", nil, 200).Settings.Version, imported), 200)
	if view.Runtime.Revision != imported {
		t.Fatal("rollback runtime revision")
	}
	complete("second-model", 404)
	complete("chat-model", 200)

	// Audit events name changed models and aliases, never credential values.
	var details string
	if err := f.pool.QueryRow(t.Context(), `SELECT string_agg(action||':'||result||':'||COALESCE(details::text,''), ' ' ORDER BY id) FROM audit_events WHERE target_id='routing'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"settings.import:succeeded", "settings.activate:refused", "settings.activate:succeeded", "models.second-model", "aliases.fast", "settings.rollback:succeeded"} {
		if !strings.Contains(details, want) {
			t.Fatal("audit", want, details)
		}
	}
	if strings.Contains(details, backendToken) {
		t.Fatal("audit exposed a backend credential")
	}
}
