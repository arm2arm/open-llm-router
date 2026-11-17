package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func TestResponsesNativeForwarding(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[stream], func(t *testing.T) {
			mock := mockllm.New(mockllm.Options{Tokens: 7})
			backend := httptest.NewServer(mock)
			defer backend.Close()
			cfg := testConfig(backend.URL)
			cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			records := make(recordChannel, 2)
			_, router := accountingRouter(t, cfg, records)
			body := `{"model":"alias","input":[{"role":"user","content":[{"type":"input_text","text":"hello world"},{"type":"input_image","image_url":"data:image/png;base64,synthetic"}]}],"max_output_tokens":10,"text":{"format":{"type":"text"}},"reasoning":{"effort":"low"},"future":{"integer":123456789012345678901234567890},"stream_options":{"include_obfuscation":false},"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
			resp, out := send(t, router.URL, "/v1/responses?future=a%2Fb", body, http.Header{"Authorization": {"Bearer synthetic-router-key"}, "Cookie": {"private=synthetic"}})
			if resp.StatusCode != 200 || !bytes.Contains(out, []byte(`"output_text"`)) {
				t.Fatalf("%d %s", resp.StatusCode, out)
			}
			record := records.next(t)
			if record.Operation != "responses" || record.Protocol != "openai" || record.Model != "chat-model" || record.InputTokens != 2 || record.OutputTokens != 7 || record.Status != "200" || record.IsStream != stream {
				t.Fatalf("%+v", record)
			}
			last := mock.Stats().Last
			if last.Path != "/v1/responses?future=a%2Fb" || last.Header.Get("Authorization") != "" || last.Header.Get("Cookie") != "" {
				t.Fatal("path or credential isolation", last)
			}
			var forwarded map[string]json.RawMessage
			_ = json.Unmarshal(last.Body, &forwarded)
			if string(forwarded["model"]) != `"chat-model"` || string(forwarded["store"]) != "false" || bytes.Contains(forwarded["stream_options"], []byte("include_usage")) || !bytes.Contains(forwarded["future"], []byte("123456789012345678901234567890")) || !bytes.Contains(forwarded["input"], []byte("data:image/png;base64,synthetic")) {
				t.Fatal("changed Responses payload", string(last.Body))
			}
		})
	}
}

func TestResponsesRefusesUnownedStateAndAmbiguousControls(t *testing.T) {
	mock := mockllm.New(mockllm.Options{})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	_, router := setup(t, backend.URL)
	for _, fields := range []string{
		`"store":true`, `"background":true`, `"previous_response_id":"resp_another_user"`, `"conversation":{"id":"conv_other"}`,
		`"input":[{"type":"item_reference","id":"item_other"}]`, `"input":[{"id":"item_other"}]`,
		`"store":false,"STORE":true`, `"store":false,"store":true`, `"previous_response_id":null,"Previous_Response_Id":"resp_other"`,
		`"input":[{"type":"item_reference","type":"message","id":"item_other","role":"user","content":"x"}]`,
		`"max_output_tokens":1,"MAX_OUTPUT_TOKENS":999`, `"max_output_tokens":1,"max_output_tokens":999`,
	} {
		resp, out := send(t, router.URL, "/v1/responses", `{"model":"alias",`+fields+`}`, nil)
		if resp.StatusCode != 400 {
			t.Fatalf("%s: %d %s", fields, resp.StatusCode, out)
		}
	}
	if mock.Stats().Requests != 0 {
		t.Fatal("invalid request reached provider", mock.Stats())
	}
}

func TestResponsesKeepsModelPermissions(t *testing.T) {
	mock := mockllm.New(mockllm.Options{})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	cfg := testConfig(backend.URL)
	cfg.Groups = map[string]config.Group{"staff": {Models: []string{}}}
	records := make(recordChannel, 1)
	_, router := accountingRouter(t, cfg, records)
	resp, _ := send(t, router.URL, "/v1/responses", `{"model":"alias","input":"hello"}`, nil)
	if resp.StatusCode != 403 || mock.Stats().Requests != 0 {
		t.Fatal(resp.StatusCode, mock.Stats())
	}
	router.Close()
	if len(records) != 0 {
		t.Fatal("recorded a locally rejected request")
	}
}

func TestResponsesStreamFailureAndIncomplete(t *testing.T) {
	for _, terminal := range []string{"", "response.incomplete", "response.failed"} {
		t.Run(terminal, func(t *testing.T) {
			wire := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"
			if terminal != "" {
				wire += "data: {\"type\":\"" + terminal + "\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}}\n\n"
			}
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			}))
			defer backend.Close()
			cfg := testConfig(backend.URL)
			cfg.Groups = map[string]config.Group{"staff": {Models: []string{"*"}}}
			records := make(recordChannel, 1)
			_, router := accountingRouter(t, cfg, records)
			resp, err := router.Client().Post(router.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"chat-model","input":"hello","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			out, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(out) != wire || (readErr == nil) != (terminal != "") {
				t.Fatal("stream framing", readErr, string(out))
			}
			record := records.next(t)
			want := "200"
			if terminal == "" {
				want = "aborted"
			} else if terminal == "response.failed" {
				want = "error"
			}
			if record.Status != want || record.InputTokens <= 0 || record.OutputTokens <= 0 {
				t.Fatalf("%+v", record)
			}
			if terminal != "" && (record.InputTokens != 10 || record.OutputTokens != 20) {
				t.Fatal("lost terminal usage", record)
			}
		})
	}
}
