package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func TestPolicyDeniesBeforePoolAcquisition(t *testing.T) {
	mock := mockllm.New(mockllm.Options{})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	for _, memberships := range [][]string{nil, {}, {"unknown"}} {
		cfg := testConfig(backend.URL)
		model := cfg.Models["chat-model"]
		model.Protocols = []string{config.ProtocolOpenAI}
		cfg.Models["chat-model"] = model
		cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
		h, router := configuredForGroups(t, cfg, memberships)
		// Even an exhausted pool must return a permission error to a caller
		// without access, including for aliases and unsupported protocols.
		var release []func()
		for lease := h.Pools.Pools["chat-model"].Acquire(nil); lease != nil; lease = h.Pools.Pools["chat-model"].Acquire(nil) {
			release = append(release, lease.Release)
		}
		for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/messages", "/v1/messages/count_tokens"} {
			resp, body := send(t, router.URL, path, `{"model":"alias","stream":true}`, nil)
			if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Retry-After") != "" {
				t.Fatalf("groups %v %s: %d %s", memberships, path, resp.StatusCode, body)
			}
		}
		for _, done := range release {
			done()
		}
		resp, err := http.Get(router.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		var list struct{ Data json.RawMessage }
		err = json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || string(list.Data) != "[]" {
			t.Fatalf("groups %v: list = %s, err=%v", memberships, list.Data, err)
		}
	}
	if mock.Stats().Requests != 0 {
		t.Fatal("a denied request reached the backend")
	}
}
