package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
)

func TestGroupPolicyWithAPIKeys(t *testing.T) {
	database := openDatabase(t, newDatabase(t).dsn)
	mock := mockllm.New(mockllm.Options{Tokens: 1})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	cfg, err := config.Parse(fmt.Appendf(nil, `models:
  chat-model:
    protocols: [openai, anthropic]
    timeout_s: 5
    pool: [{url: %s, weight: 1, max_inflight: 4}]
  staff-model:
    protocols: [openai, anthropic]
    upstream_model: chat-model
    timeout_s: 5
    pool: [{url: %s, weight: 1, max_inflight: 4}]
groups:
  students: {models: [chat-model]}
  staff: {models: [staff-model]}
  admins: {models: ["*"]}
aliases:
  student-alias: chat-model
  staff-alias: staff-model
`, backend.URL, backend.URL), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.New(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()
	forwarder := proxy.New(cfg)
	defer forwarder.Close()
	forwarder.Pools.Check(t.Context())
	s := server.New(slog.New(slog.DiscardHandler))
	s.SetAPIMiddleware(authenticator.Middleware)
	forwarder.Register(s.Handle)
	router := httptest.NewServer(s.Handler())
	defer router.Close()
	admin := auth.Admin{Pool: database}
	for _, tc := range []struct {
		name   string
		groups []string
		models []string
	}{
		{"students", []string{"students"}, []string{"chat-model", "student-alias"}},
		{"staff", []string{"staff"}, []string{"staff-alias", "staff-model"}},
		{"combined", []string{"students", "staff"}, []string{"chat-model", "staff-alias", "staff-model", "student-alias"}},
		{"wildcard", []string{"admins"}, []string{"chat-model", "staff-alias", "staff-model", "student-alias"}},
		{"unmatched", []string{"unknown"}, nil},
		{"no groups", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, err := admin.AddUser(t.Context(), tc.name, "", tc.groups)
			if err != nil {
				t.Fatal(err)
			}
			key, err := admin.CreateKey(t.Context(), owner, "policy-test", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			request := func(path, model string) (*http.Response, []byte) {
				t.Helper()
				method, header, credential := "POST", "Authorization", "Bearer "+key
				if model == "" {
					method = "GET"
				}
				if strings.Contains(path, "/messages") {
					header, credential = "X-Api-Key", key
				}
				req, err := http.NewRequestWithContext(t.Context(), method, router.URL+path,
					strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"prompt":"hello","input":"hello","max_tokens":1}`, model)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set(header, credential)
				req.Header.Set("Content-Type", "application/json")
				resp, err := router.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				return resp, body
			}
			resp, body := request("/v1/models", "")
			var list struct {
				Object string
				Data   []struct{ ID string }
			}
			if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != http.StatusOK || list.Object != "list" || list.Data == nil {
				t.Fatalf("model list: %d %s err=%v", resp.StatusCode, body, err)
			}
			var names []string
			for _, model := range list.Data {
				names = append(names, model.ID)
			}
			if !slices.Equal(names, tc.models) {
				t.Fatalf("listed models = %v, want %v", names, tc.models)
			}
			for _, model := range []string{"chat-model", "student-alias", "staff-model", "staff-alias"} {
				for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/messages", "/v1/messages/count_tokens"} {
					before := mock.Stats().Requests
					resp, body := request(path, model)
					if slices.Contains(tc.models, model) {
						if resp.StatusCode != http.StatusOK || mock.Stats().Requests != before+1 {
							t.Fatalf("%s %s: %d %s", path, model, resp.StatusCode, body)
						}
						continue
					}
					var shape struct {
						Type  string
						Error map[string]any
					}
					if json.Unmarshal(body, &shape) != nil || resp.StatusCode != http.StatusForbidden || shape.Error["type"] != "permission_error" || shape.Error["message"] == "" ||
						resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("X-Request-Id") == "" {
						t.Fatalf("%s %s: expected permission error, got %d %s", path, model, resp.StatusCode, body)
					}
					if strings.Contains(path, "/messages") {
						if shape.Type != "error" || len(shape.Error) != 2 {
							t.Fatalf("Anthropic error: %s", body)
						}
					} else if shape.Type != "" || shape.Error["code"] != float64(403) || len(shape.Error) != 4 {
						t.Fatalf("OpenAI error: %s", body)
					}
					if mock.Stats().Requests != before {
						t.Fatal("a forbidden model reached the backend")
					}
				}
			}
		})
	}
}
