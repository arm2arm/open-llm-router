package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/policy"
)

func TestModelAPIRecipientsUseActiveRoutingAndAccess(t *testing.T) {
	h := New(testConfig())
	defer h.Close()
	cfg := testConfig("http://unhealthy.internal")
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
	cfg.Backends = map[string]config.BackendPolicy{"private": {Groups: []string{}}}
	m := cfg.Models["chat-model"]
	m.Pool[0].PublicOrganization, m.Pool[0].PrivacyURL = "Institute", "https://example.org/privacy"
	m.Pool = append(m.Pool, config.Replica{Backend: "private", PublicOrganization: "Private organization", URL: "http://private.internal"})
	cfg.Models["chat-model"] = m
	cfg.Models["restricted"] = config.Model{Pool: []config.Replica{m.Pool[1]}}
	h.routing.Store(&routingSnapshot{config: cfg})
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r = r.WithContext(auth.NewContext(r.Context(), auth.Principal{Groups: []string{"staff"}}))
	w := httptest.NewRecorder()
	h.models(w, r)
	var body struct {
		Data []struct {
			ID         string             `json:"id"`
			Recipients []policy.Recipient `json:"recipient_details"`
			Complete   bool               `json:"recipients_complete"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Data) != 2 {
		t.Fatal(w.Body.String())
	}
	for _, model := range body.Data {
		if !model.Complete || len(model.Recipients) != 1 || model.Recipients[0].Organization != "Institute" {
			t.Fatal(w.Body.String())
		}
	}
	for _, secret := range []string{".internal", "Private organization", "restricted"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("catalogue leaked %s", secret)
		}
	}
}

func TestProvenanceThroughProxyAndAccounting(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":12,"completion_tokens":7}`
	const marker = `,"provenance":{"marker":"synthetic-test-only","number":123456789012345678901234567890}`
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		for _, stream := range []bool{false, true} {
			for _, supplemental := range []bool{false, true} {
				for _, asked := range []bool{false, true} {
					extra := ""
					if supplemental {
						extra = marker
					}
					payload := `{"choices":[],` + usage + extra + `}`
					wire, want := payload, payload
					if stream {
						wire = "data: " + payload + "\n\ndata: [DONE]\n\n"
						want = wire
						if !supplemental && !asked {
							want = "data: [DONE]\n\n"
						}
					}
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if backendProbe(r) {
							return
						}
						var request struct {
							StreamOptions struct {
								IncludeUsage bool `json:"include_usage"`
							} `json:"stream_options"`
						}
						if json.NewDecoder(r.Body).Decode(&request) != nil || stream && !request.StreamOptions.IncludeUsage {
							t.Error("missing injected usage request")
						}
						w.Header().Set("Content-Type", "application/json")
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						_, _ = w.Write([]byte(wire))
					}))
					cfg := testConfig(backend.URL)
					cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
					records := make(recordChannel, 1)
					_, router := accountingRouter(t, cfg, records)
					request, _ := json.Marshal(map[string]any{"model": "alias", "stream": stream, "stream_options": map[string]bool{"include_usage": asked}})
					resp, body := send(t, router.URL, path, string(request), nil)
					if resp.StatusCode != 200 || string(body) != want {
						t.Fatalf("%s stream=%t marker=%t asked=%t: %d %s", path, stream, supplemental, asked, resp.StatusCode, body)
					}
					record := records.next(t)
					if record.InputTokens != 12 || record.OutputTokens != 7 {
						t.Fatal("provenance changed accounting", record)
					}
					router.Close()
					backend.Close()
				}
			}
		}
	}
}
