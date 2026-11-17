package requesttrace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestResponsesTraceToolLoopAndPrivacy(t *testing.T) {
	c := &Capture{operation: "responses", jsonResponse: true}
	c.Input([]byte(`{"model":"test","instructions":"Be concise","input":[{"role":"user","content":[{"type":"input_text","text":"weather?"},{"type":"input_image","image_url":"https://example.invalid/private-image"}]},{"type":"reasoning","encrypted_content":"private-encrypted","summary":[{"type":"summary_text","text":"Think"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"sunny"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`))
	c.Write([]byte(`{"id":"resp_1","model":"test","status":"completed","output":[{"type":"reasoning","encrypted_content":"private-output","summary":[{"type":"summary_text","text":"Conclude"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Sunny"}]}],"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":90},"output_tokens_details":{"reasoning_tokens":5}}}`))
	d := c.decode()
	raw, _ := json.Marshal(d)
	for _, secret := range []string{"private-image", "private-encrypted", "private-output"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("stored excluded payload", secret)
		}
	}
	if len(d.Input) != 5 || d.Summary.ToolResults != 1 || d.Summary.CachedTokens != 90 || d.Summary.ReasoningTokens != 5 || len(d.Output) != 2 || d.Output[1].Blocks[0].Text != "Sunny" || d.Tools[0].Name != "weather" {
		t.Fatalf("%s", raw)
	}
	if d.Input[0].Role != "system" || d.Input[3].Blocks[0].ID != "call_1" || d.Input[4].Blocks[0].Text != "sunny" {
		t.Fatalf("input %+v", d.Input)
	}
}

func TestResponsesTraceStreamingAndFinalOnly(t *testing.T) {
	for _, finalOnly := range []bool{false, true} {
		c := &Capture{operation: "responses", began: time.Now(), record: usage.Record{IsStream: true}}
		events := []string{}
		if !finalOnly {
			events = append(events,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"weather","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{"}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"}"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{}"}}`)
		}
		events = append(events, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"weather","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":7}}}`)
		wire := ""
		for _, event := range events {
			wire += "data: " + event + "\n\n"
		}
		for i := 0; i < len(wire); i += 7 {
			c.Write([]byte(wire[i:min(i+7, len(wire))]))
		}
		d := c.decode()
		if (d.Summary.ToolCalls == nil || *d.Summary.ToolCalls != 1) || len(d.Output) != 1 || d.Output[0].Blocks[0].Arguments != "{}" || d.Output[0].Blocks[0].ID != "call_1" || d.Result.ID != "resp_1" || d.Result.Usage["output_tokens"] != 7 {
			t.Fatalf("final-only=%v %+v", finalOnly, d)
		}
	}
}
