package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The backend cannot finish until the client receives the first event. This
// proves incremental delivery, including thinking/tool blocks, without a speed
// threshold. Image content and feature controls must survive model rewriting.
func TestStreamingMultimodalCapabilities(t *testing.T) {
	for _, tc := range []struct{ name, path, body, first, last string }{
		{"openai", "/v1/chat/completions", `{"model":"alias","stream":true,"reasoning_effort":"low","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,fixture"}}]}],"tools":[{"type":"function","function":{"name":"weather"}}]}`,
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Thinking\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"},
		{"anthropic", "/v1/messages", `{"model":"alias","stream":true,"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":512},"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"fixture"}}]}],"tools":[{"name":"weather","input_schema":{"type":"object"}}]}`,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Thinking\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					return
				}
				body, _ := io.ReadAll(r.Body)
				var before, after map[string]json.RawMessage
				_ = json.Unmarshal([]byte(tc.body), &before)
				_ = json.Unmarshal(body, &after)
				for _, field := range []string{"messages", "tools", "thinking", "reasoning_effort"} {
					if string(after[field]) != string(before[field]) {
						t.Errorf("changed %s", field)
					}
				}
				if string(after["model"]) != `"chat-model"` {
					t.Error("alias not rewritten")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.first)
				_ = http.NewResponseController(w).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, tc.last)
			}))
			defer func() { unblock(); backend.Close() }()
			_, router := setup(t, backend.URL)
			client := &http.Client{Timeout: 4 * time.Second}
			resp, err := client.Post(router.URL+tc.path, "application/json", strings.NewReader(tc.body))
			if err != nil {
				unblock()
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			var first strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					unblock()
					t.Fatal("first event was buffered", err)
				}
				first.WriteString(line)
				if line == "\n" {
					break
				}
			}
			if first.String() != tc.first {
				t.Fatal("thinking event altered")
			}
			unblock()
			rest, err := io.ReadAll(reader)
			if err != nil || string(rest) != tc.last || resp.StatusCode != 200 {
				t.Fatal("tool stream changed or truncated", err)
			}
		})
	}
}
