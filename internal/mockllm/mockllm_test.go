package mockllm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOptionsFromEnv(t *testing.T) {
	env := map[string]string{
		"MOCK_MODELS":      " chat-model, embed-model ",
		"MOCK_TOKENS":      "7",
		"MOCK_TOKEN_DELAY": "2ms",
		"MOCK_FAIL":        "error",
		"MOCK_THINKING":    "true",
	}
	opts, err := OptionsFromEnv(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	want := Options{
		Models: []string{"chat-model", "embed-model"}, Tokens: 7,
		TokenDelay: 2 * time.Millisecond, Fail: FailError, Thinking: true,
	}
	if !reflect.DeepEqual(opts, want) {
		t.Fatalf("options = %#v, want %#v", opts, want)
	}

	env["MOCK_TOKEN_DELAY"] = "yesterday"
	if _, err := OptionsFromEnv(func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "MOCK_TOKEN_DELAY") {
		t.Fatalf("invalid duration error = %v", err)
	}
}

func TestOpenAIResponsesAndStats(t *testing.T) {
	s := httptest.NewServer(New(Options{Models: []string{"chat-model", "embed-model"}, Tokens: 5}))
	defer s.Close()

	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	postJSON(t, s.URL+"/v1/chat/completions", map[string]any{
		"model": "chat-model", "messages": []any{map[string]any{"role": "user", "content": "one two three"}},
		"max_tokens": 2,
	}, nil, http.StatusOK, &chat)
	if got := chat.Choices[0]; got.Message.Content != "The quick" || got.FinishReason != "length" {
		t.Fatalf("chat choice = %#v", got)
	}
	if got := chat.Usage; got.Prompt != 3 || got.Completion != 2 || got.Total != 5 {
		t.Fatalf("usage = %#v", got)
	}

	var embeddings struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			Prompt int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	postJSON(t, s.URL+"/v1/embeddings", map[string]any{
		"model": "embed-model", "input": []string{"first text", "second text"}, "dimensions": 4,
	}, nil, http.StatusOK, &embeddings)
	if len(embeddings.Data) != 2 || len(embeddings.Data[0].Embedding) != 4 {
		t.Fatalf("embedding dimensions = %#v", embeddings.Data)
	}
	if reflect.DeepEqual(embeddings.Data[0].Embedding, embeddings.Data[1].Embedding) {
		t.Fatal("different inputs produced equal embeddings")
	}
	if embeddings.Usage.Prompt != 4 {
		t.Fatalf("embedding input tokens = %d, want 4", embeddings.Usage.Prompt)
	}

	var stats Stats
	getJSON(t, s.URL+"/_mock/stats", http.StatusOK, &stats)
	if stats.Requests != 2 || stats.Inflight != 0 || stats.PeakInflight != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	if stats.Last == nil || stats.Last.Path != "/v1/embeddings" || !bytes.Contains(stats.Last.Body, []byte("embed-model")) {
		t.Fatalf("last request = %#v", stats.Last)
	}

	req, err := http.NewRequest(http.MethodPost, s.URL+"/_mock/reset", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status = %d", res.StatusCode)
	}
	getJSON(t, s.URL+"/_mock/stats", http.StatusOK, &stats)
	if stats.Requests != 0 || stats.Last != nil {
		t.Fatalf("stats after reset = %#v", stats)
	}
}

func TestOpenAIStreamsTextToolAndUsage(t *testing.T) {
	s := httptest.NewServer(New(Options{Tokens: 3}))
	defer s.Close()

	text := postText(t, s.URL+"/v1/chat/completions", map[string]any{
		"model": "chat-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"stream": true, "stream_options": map[string]any{"include_usage": true},
	}, nil, http.StatusOK)
	if strings.Count(text, "data: ") != 7 || !strings.Contains(text, `"completion_tokens":3`) || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("unexpected text stream:\n%s", text)
	}

	tool := postText(t, s.URL+"/v1/chat/completions", map[string]any{
		"model": "chat-model", "messages": []any{map[string]any{"role": "user", "content": "weather"}},
		"stream": true,
		"tools":  []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}},
	}, nil, http.StatusOK)
	for _, want := range []string{`"name":"get_weather"`, `"arguments":"{"`, `"arguments":"}"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(tool, want) {
			t.Fatalf("tool stream does not contain %q:\n%s", want, tool)
		}
	}
}

func TestAnthropicResponsesAndEventSequence(t *testing.T) {
	s := httptest.NewServer(New(Options{Tokens: 4}))
	defer s.Close()

	var count struct {
		InputTokens int `json:"input_tokens"`
	}
	postJSON(t, s.URL+"/v1/messages/count_tokens", map[string]any{
		"model": "chat-model", "system": "be concise", "messages": []any{map[string]any{"role": "user", "content": "one two three"}},
	}, nil, http.StatusOK, &count)
	if count.InputTokens != 5 {
		t.Fatalf("input tokens = %d, want 5", count.InputTokens)
	}

	stream := postText(t, s.URL+"/v1/messages", map[string]any{
		"model": "chat-model", "max_tokens": 20, "stream": true,
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 8},
		"messages": []any{map[string]any{"role": "user", "content": "weather"}},
		"tools":    []any{map[string]any{"name": "get_weather", "input_schema": map[string]any{"type": "object"}}},
	}, nil, http.StatusOK)
	events := eventNames(stream)
	want := []string{
		"message_start",
		"content_block_start", "ping", "content_block_delta", "content_block_delta", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v\n%s", events, want, stream)
	}
	for _, part := range []string{`"type":"thinking"`, `"type":"signature_delta"`, `"type":"tool_use"`, `"stop_reason":"tool_use"`, `"output_tokens":5`} {
		if !strings.Contains(stream, part) {
			t.Fatalf("Anthropic stream does not contain %q:\n%s", part, stream)
		}
	}
}

func TestFaultInjection(t *testing.T) {
	s := httptest.NewServer(New(Options{}))
	defer s.Close()

	req, err := http.NewRequest(http.MethodGet, s.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Mock-Fail", "unhealthy")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy status = %d", res.StatusCode)
	}

	errorBody := postText(t, s.URL+"/v1/chat/completions", chatBody(false), map[string]string{"X-Mock-Fail": "error"}, http.StatusInternalServerError)
	if !strings.Contains(errorBody, "mock failure") {
		t.Fatalf("500 body = %s", errorBody)
	}

	_, err = post(s.URL+"/v1/chat/completions", chatBody(false), map[string]string{"X-Mock-Fail": "drop"})
	if err == nil {
		t.Fatal("connection drop returned no error")
	}

	res, err = post(s.URL+"/v1/chat/completions", chatBody(false), map[string]string{"X-Mock-Fail": "drop-mid-stream"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err == nil {
		t.Fatal("mid-response connection drop returned a complete body")
	}
}

func TestClosingStreamIncrementsAbortedWithinOneSecond(t *testing.T) {
	s := httptest.NewServer(New(Options{Tokens: 20, TokenDelay: 100 * time.Millisecond}))
	defer s.Close()

	res, err := post(s.URL+"/v1/chat/completions", chatBody(true), nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var stats Stats
		getJSON(t, s.URL+"/_mock/stats", http.StatusOK, &stats)
		if stats.StreamsAborted == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	var stats Stats
	getJSON(t, s.URL+"/_mock/stats", http.StatusOK, &stats)
	t.Fatalf("streams_aborted = %d after one second", stats.StreamsAborted)
}

func chatBody(stream bool) map[string]any {
	return map[string]any{
		"model": "chat-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": stream,
	}
}

func post(url string, body any, headers map[string]string) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return http.DefaultClient.Do(req)
}

func postText(t *testing.T, url string, body any, headers map[string]string, status int) string {
	t.Helper()
	res, err := post(url, body, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != status {
		t.Fatalf("POST %s status = %d, want %d; body = %s", url, res.StatusCode, status, b)
	}
	return string(b)
}

func postJSON(t *testing.T, url string, body any, headers map[string]string, status int, dst any) {
	t.Helper()
	text := postText(t, url, body, headers, status)
	if err := json.Unmarshal([]byte(text), dst); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
}

func getJSON(t *testing.T, url string, status int, dst any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != status {
		t.Fatalf("GET %s status = %d, want %d", url, res.StatusCode, status)
	}
	if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}

func eventNames(stream string) []string {
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(stream))
	for scanner.Scan() {
		if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
			events = append(events, name)
		}
	}
	return events
}
