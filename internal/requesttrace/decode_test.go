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
	c.Input([]byte(`{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"hello <script>alert(1)</script>"},{"type":"image_url","image_url":{"url":"https://private-image?token=secret-image"}}]},{"role":"assistant","tool_calls":[{"id":"call-1","function":{"name":"weather","arguments":"{\"city\":\"Berlin\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"sunny"}],"temperature":0.5,"tools":[{"type":"function","function":{"name":"weather","parameters":{"secret-schema":"not-stored"}}}],"metadata":{"secret":"not-stored"}}`))
	c.Write([]byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"think","content":"hello","tool_calls":[{"id":"call-2","function":{"name":"search","arguments":"{\"q\":\"abc\"}"}}]},"finish_reason":"tool_calls"}],"provider_metadata":{"private":"not-stored"}}`))
	d := c.decode()
	b, _ := json.Marshal(d)
	for _, secret := range []string{"secret-image", "not-stored", "provider_metadata", "secret-schema"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("stored omitted data: %s", secret)
		}
	}
	if len(d.Input) != 3 || d.Input[0].Blocks[1].Type != "image" || d.Input[2].Blocks[0].Type != "tool_result" || len(d.Output) != 1 || d.Output[0].Blocks[0].Type != "reasoning" || d.Output[0].Blocks[2].Arguments != `{"q":"abc"}` {
		t.Fatalf("unexpected trace: %s", b)
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
	s := &Service{slots: make(chan struct{}, Slots), queue: make(chan *Capture, Slots)}
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
