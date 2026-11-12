package requesttrace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestOpenAITraceContentAndOmissions(t *testing.T) {
	c := &Capture{operation: "chat/completions", jsonResponse: true}
	c.Input([]byte(`{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"hello <script>alert(1)</script>"},{"type":"image_url","image_url":{"url":"https://private-image?token=secret-image"}}]},{"role":"assistant","tool_calls":[{"id":"call-1","function":{"name":"weather","arguments":"{\"city\":\"Berlin\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"sunny"}],"temperature":0.5,"tools":[{"type":"function","function":{"name":"weather","description":"Look up weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"metadata":{"secret":"not-stored"}}`))
	c.Write([]byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"think","content":"hello","tool_calls":[{"id":"call-2","function":{"name":"search","arguments":"{\"q\":\"abc\"}"}}]},"finish_reason":"tool_calls"}],"provider_metadata":{"private":"not-stored"}}`))
	d := c.decode()
	b, _ := json.Marshal(d)
	for _, secret := range []string{"secret-image", "not-stored", "private"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("stored omitted data: %s", secret)
		}
	}
	if len(d.Input) != 3 || d.Input[0].Blocks[1].Type != "image" || d.Input[2].Blocks[0].Type != "tool_result" || len(d.Output) != 1 || d.Output[0].Blocks[0].Type != "reasoning" || d.Output[0].Blocks[2].Arguments != `{"q":"abc"}` {
		t.Fatalf("unexpected trace: %s", b)
	}
	if len(d.Tools) != 1 || d.Tools[0].Name != "weather" || !strings.Contains(string(d.Tools[0].Schema), `"city"`) || d.Parameters["temperature"] != 0.5 {
		t.Fatalf("tools/parameters: %s", b)
	}
	if !strings.Contains(string(d.Request), `"temperature":0.5`) || !strings.Contains(string(d.Request), "image payload omitted") || !strings.Contains(string(d.Response), `"finish_reason":"tool_calls"`) {
		t.Fatalf("raw previews: %s", b)
	}
	if d.Summary.ToolCalls != 1 || d.Summary.ToolResults != 1 || d.Summary.FinishReason != "tool_calls" || d.Version != DetailVersion {
		t.Fatalf("summary: %+v", d.Summary)
	}
	if d.Input[0].Hash == "" || d.Input[0].Hash == d.Input[1].Hash {
		t.Fatal("message hashes")
	}
}

func TestStreamReconstruction(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(strings.ReplaceAll(ending, "\r", "CR"), func(t *testing.T) {
			c := &Capture{began: time.Now(), record: usage.Record{IsStream: true, LatencyMS: 20}}
			events := []string{`{"choices":[{"index":0,"delta":{"reasoning_content":"let me think"}}]}`, `{"choices":[{"index":0,"delta":{"content":"Hello "}}]}`, `{"choices":[{"index":0,"delta":{"content":"world","tool_calls":[{"index":0,"id":"call-1","function":{"name":"weather","arguments":"{\"ci"}}]}}]}`, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Berlin\"}"}}]}}]}`, `[DONE]`}
			wire := ""
			for _, e := range events {
				wire += "data: " + e + ending + ending
			}
			for i := 0; i < len(wire); i += 7 {
				end := min(i+7, len(wire))
				c.Write([]byte(wire[i:end]))
			}
			d := c.decode()
			if len(d.Output) != 1 || len(d.Output[0].Blocks) != 3 || d.Output[0].Blocks[0].Text != "let me think" || d.Output[0].Blocks[1].Text != "Hello world" || d.Output[0].Blocks[2].Arguments != `{"city":"Berlin"}` || d.Events[len(d.Events)-1].Type != "complete" {
				t.Fatalf("reconstruction: %+v", d)
			}
		})
	}
}

func TestAnthropicStreamAndPartialTail(t *testing.T) {
	c := &Capture{began: time.Now(), record: usage.Record{IsStream: true}}
	for _, data := range []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Reason"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"secret-signature"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-1","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":1}"}}`,
	} {
		c.Write([]byte("data: " + data + "\n\n"))
	}
	c.Write([]byte("data: {\"type\":\"unfinished-secret"))
	d := c.decode()
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "secret") || len(d.Output) != 1 || d.Output[0].Blocks[0].Text != "Reason" || d.Output[0].Blocks[1].Arguments != `{"id":1}` {
		t.Fatalf("unexpected trace %s", raw)
	}
}

func TestCaptureBoundsAndNonblockingAdmission(t *testing.T) {
	s := &Service{enabled: true, slots: make(chan struct{}, Slots), queue: make(chan *Capture, Slots)}
	consent := map[string]Preferences{"user": {Enabled: true, RetentionDays: 7}}
	s.prefs.Store(&consent)
	for range Slots {
		c := s.Begin("user", "chat", time.Now())
		if c == nil {
			t.Fatal("unexpected rejection")
		}
		c.Input([]byte(strings.Repeat("x", MaxInput+1)))
		c.Write([]byte("data: [DONE]\n\n"))
		c.Finish(usage.Record{})
	}
	if s.Begin("user", "chat", time.Now()) != nil || s.Dropped.Load() != 1 {
		t.Fatal("capacity must drop without waiting")
	}
	c := <-s.queue
	if len(c.input) != 0 || len(c.output) == 0 || !c.truncated {
		t.Fatal("oversized input must not suppress output")
	}
	c = &Capture{}
	for range MaxChunks + 5 {
		c.Write([]byte("x"))
	}
	if len(c.chunks) != MaxChunks || !c.truncated {
		t.Fatal("unbounded chunks")
	}
	c = &Capture{}
	c.Write([]byte(strings.Repeat("a", MaxOutput)))
	c.Write([]byte("overflow"))
	c.Write([]byte("later"))
	if len(c.output) != MaxOutput || !c.truncated {
		t.Fatal("unbounded output")
	}
	p := map[string]Preferences{"off": {Enabled: false, RetentionDays: 7}}
	s.prefs.Store(&p)
	if s.Begin("off", "chat", time.Now()) != nil {
		t.Fatal("disabled user captured")
	}
}

func BenchmarkCaptureForwarding(b *testing.B) {
	input := []byte(`{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	chunk := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello world\"}}]}\n\n")
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var c *Capture
				if enabled {
					c = &Capture{began: time.Now()}
				}
				c.Input(input)
				for range 32 {
					c.Write(chunk)
				}
			}
		})
	}
}

func TestRawPreviewRemovesPayloadsAndBoundsStrings(t *testing.T) {
	long := strings.Repeat("é", 3000)
	b64 := strings.Repeat("QUJD", 200)
	vector := "[" + strings.TrimSuffix(strings.Repeat("0.1,", 40), ",") + "]"
	c := &Capture{operation: "messages", jsonResponse: true}
	c.Input([]byte(`{"model":"m","system":"` + long + `","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"` + b64 + `"}},{"type":"text","text":"data:image/png;base64,` + b64 + `"}]}],"thinking":{"type":"enabled","budget_tokens":1024},"tool_choice":{"type":"auto"},"stop_sequences":["END"],"weights":` + vector + `}`))
	c.Write([]byte(`{"id":"msg_1","model":"served","content":[{"type":"thinking","thinking":"hm","signature":"sig-secret"},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":4}}`))
	d := c.decode()
	raw, _ := json.Marshal(d)
	for _, leak := range []string{b64, "sig-secret"} {
		if strings.Contains(string(raw), leak) {
			t.Fatal("raw preview leaked a payload")
		}
	}
	if !strings.Contains(string(d.Request), "+1" /* +1xxx chars */) || !strings.Contains(string(d.Request), "40 numbers omitted") || len(d.Input[0].Blocks[0].Text) != len(long) {
		t.Fatalf("string bounds: %s", d.Request)
	}
	if d.Parameters["tool_choice"] == nil || d.Parameters["stop_sequences"] == nil || object(d.Parameters["thinking"])["budget_tokens"] != 1024.0 {
		t.Fatalf("parameters: %+v", d.Parameters)
	}
	if d.Result.ID != "msg_1" || d.Result.Model != "served" || d.Summary.FinishReason != "end_turn" || d.Summary.CachedTokens != 4 || d.Result.Usage["input_tokens"] != 10 {
		t.Fatalf("result: %+v %+v", d.Result, d.Summary)
	}
}

func TestStreamFramesAndResult(t *testing.T) {
	c := &Capture{began: time.Now(), record: usage.Record{IsStream: true}}
	for _, data := range []string{
		`{"id":"chatcmpl-1","model":"served","system_fingerprint":"fp","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"completion_tokens_details":{"reasoning_tokens":2},"prompt_tokens_details":{"cached_tokens":3}}}`,
		`[DONE]`,
	} {
		c.Write([]byte("data: " + data + "\n\n"))
	}
	d := c.decode()
	if len(d.Frames) != 4 || string(d.Frames[3].Data) != `"[DONE]"` || d.Result.ID != "chatcmpl-1" || d.Result.SystemFingerprint != "fp" || d.Summary.FinishReason != "stop" || d.Summary.ReasoningTokens != 2 || d.Summary.CachedTokens != 3 {
		t.Fatalf("frames/result: %+v %+v", d.Frames, d.Summary)
	}
	c = &Capture{began: time.Now(), record: usage.Record{IsStream: true}}
	for range MaxFrames + 10 {
		c.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"x"}}]}` + "\n\n"))
	}
	if d = c.decode(); len(d.Frames) != MaxFrames || d.Output[0].Blocks[0].Text != strings.Repeat("x", MaxFrames+10) {
		t.Fatal("frame bound must not limit reconstruction")
	}
}

func TestRoutingAndInputBudget(t *testing.T) {
	c := &Capture{operation: "chat/completions", jsonResponse: true}
	c.Route("alias", "canonical", "served")
	c.Change("model", "replaced", "alias", "served")
	c.Admission(3 * time.Millisecond)
	c.Attempt(Attempt{Replica: "replica 1 of 2", Error: "backend connection failed before the response started"})
	c.Attempt(Attempt{Replica: "replica 2 of 2", Status: 200})
	c.Headers(map[string][]string{"Traceparent": {"secret-value"}, "Content-Type": {"application/json"}})
	c.Upstream(200, "application/json")
	text := strings.Repeat("a ", MaxInputText/2+100)
	c.Input([]byte(`{"model":"alias","messages":[{"role":"user","content":"` + text + `"}]}`))
	d := c.decode()
	raw, _ := json.Marshal(d)
	if strings.Contains(string(raw), "secret-value") || len(d.Routing.Attempts) != 2 || d.Routing.Headers[0] != "Content-Type" || d.Routing.Changes[0].To != "served" || d.Routing.UpstreamModel != "served" {
		t.Fatalf("routing: %+v", d.Routing)
	}
	if !d.Truncated || len(d.Input[0].Blocks[0].Text) > MaxInputText+64 || d.Request == nil {
		t.Fatal("input budget")
	}
}

func TestConversationKey(t *testing.T) {
	key := func(user, session, body string) string {
		c := &Capture{operation: "chat/completions", session: session, record: usage.Record{UserID: user}}
		c.Input([]byte(body))
		return c.conversationKey(c.decode())
	}
	first := `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"task"}]}`
	next := `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"task"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}]}`
	if k := key("u1", "", first); k == "" || k != key("u1", "", next) || k == key("u2", "", first) || k == key("u1", "", `{"messages":[{"role":"user","content":"other"}]}`) {
		t.Fatal("content key")
	}
	if k := key("u1", "abc", first); k[0] != 's' || k != key("u1", "abc", `{"messages":[{"role":"user","content":"other"}]}`) {
		t.Fatal("session key")
	}
	if key("u1", "", `{"model":"m"}`) != "" {
		t.Fatal("no messages")
	}
}
