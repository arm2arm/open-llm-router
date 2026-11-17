package requesttrace

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestTracePreservesExactNumbersAndRemovesNUL(t *testing.T) {
	c := &Capture{operation: "chat/completions", jsonResponse: true}
	c.Input([]byte(`{"model":"test","seed":1234567890123456789,"nested":{"counter":1234567890123456789},"nu\u0000l":"x\u0000y","messages":[{"role":"user","content":"hello\u0000world"}]}`))
	c.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer\u0000text"}}],"extension":1234567890123456789}`))
	d := c.decode()
	if d.Parameters["seed"] != json.Number("1234567890123456789") || object(d.Parameters["nested"])["counter"] != json.Number("1234567890123456789") {
		t.Fatal("numeric parameters rounded", d.Parameters)
	}
	r, _ := newRedactor(nil)
	raw, err := r.marshal(d)
	if err != nil || strings.Contains(string(raw), `\u0000`) || strings.ContainsRune(string(raw), '\x00') {
		t.Fatal("NUL escaped the storage boundary", string(raw), err)
	}
	if strings.Count(string(raw), "1234567890123456789") != 5 || !strings.Contains(string(raw), "hello�world") || !strings.Contains(string(raw), "answer�text") || !strings.Contains(string(raw), "nu�l") {
		t.Fatal("preview numbers or NUL replacement changed", string(raw))
	}
}

func TestTraceRedactsStructuredCredentialsAndValidArguments(t *testing.T) {
	c := &Capture{operation: "chat/completions", jsonResponse: true}
	c.Input([]byte(`{"model":"test","password":"request-password","api_key":"request-api","access_token":"request-access","secret":"request-secret","messages":[{"role":"assistant","tool_calls":[{"function":{"name":"tool","arguments":"{\"password\":\"argument-password\",\"seed\":1234567890123456789,\"binary\":\"data:image/png;base64,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\"}"}}]}]}`))
	c.Write([]byte(`{"secret":"response-secret","choices":[{"message":{"role":"assistant","content":"safe","tool_calls":[{"function":{"name":"tool","arguments":"{\"access_token\":\"argument-token\",\"ok\":true}"}}]}}]}`))
	r, _ := newRedactor(nil)
	raw, err := r.marshal(c.decode())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"request-password", "request-api", "request-access", "request-secret", "argument-password", "response-secret", "argument-token", "AAAAAAAAAAAA"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential/payload escaped", secret, string(raw))
		}
	}
	var detail Detail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	for _, messages := range [][]Message{detail.Input, detail.Output} {
		for _, message := range messages {
			for _, block := range message.Blocks {
				if block.Type == "tool_call" && !json.Valid([]byte(block.Arguments)) {
					t.Fatal("redaction broke tool JSON", block.Arguments)
				}
			}
		}
	}
}

func TestNumericCredentialUsageCannotBreakTraceReads(t *testing.T) {
	c := &Capture{operation: "chat/completions", jsonResponse: true}
	c.Write([]byte(`{"usage":{"password":123456,"nested":{"api_key":456789},"completion_tokens":4},"choices":[]}`))
	r, _ := newRedactor(nil)
	raw, err := r.marshal(c.decode())
	if err != nil {
		t.Fatal(err)
	}
	var detail Detail
	if err := decodeJSON(raw, &detail); err != nil {
		t.Fatal("sanitized numeric usage must remain readable", err)
	}
	if len(detail.Result.Usage) != 1 || detail.Result.Usage["completion_tokens"] != 4 {
		t.Fatal("credential fields entered numeric statistics", detail.Result.Usage)
	}
}

func TestSplitStreamSecretsCannotEscapeThroughFramesOrEvents(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			c := &Capture{operation: "chat/completions", record: usage.Record{IsStream: true}}
			if protocol == "responses" {
				c.operation = "responses"
			}
			// Split at every byte: no individual event can match either regex.
			for _, char := range "Bearer top-secret-token ORG-123456" {
				var event string
				switch protocol {
				case "openai":
					event = fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, string(char))
				case "anthropic":
					event = fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, string(char))
				case "responses":
					event = fmt.Sprintf(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":%q}`, string(char))
				}
				c.Write([]byte("data: " + event + "\n\n"))
			}
			c.Write([]byte("data: [DONE]\n\n"))
			r, _ := newRedactor([]string{`ORG-[0-9]{6}`})
			raw, err := r.marshal(c.decode())
			if err != nil || strings.Contains(string(raw), "top-secret-token") || strings.Contains(string(raw), "ORG-123456") {
				t.Fatal("joined secret leaked", string(raw), err)
			}
			var d Detail
			if err := json.Unmarshal(raw, &d); err != nil || len(d.Output) != 1 || !strings.Contains(d.Output[0].Blocks[0].Text, "[REDACTED]") {
				t.Fatal("missing reconstructed redacted output", d, err)
			}
			for _, event := range d.Events {
				if event.Text != "" {
					t.Fatal("timeline retained unsafe fragments", event)
				}
			}
			for _, frame := range d.Frames {
				if string(frame.Data) != `"[DONE]"` && !strings.Contains(string(frame.Data), "stream text omitted") {
					t.Fatal("raw frame retained unsafe fragments", string(frame.Data))
				}
			}
		})
	}
}

func nestedToolResults(depth int, text string) string {
	value := `[{"type":"text","text":` + fmt.Sprintf("%q", text) + `}]`
	return strings.Repeat(`[{"type":"tool_result","tool_use_id":"call","content":`, depth) + value + strings.Repeat(`}]`, depth)
}

func TestNestedToolResultsAreBoundedAndOrdinaryResultsStillRender(t *testing.T) {
	for _, depth := range []int{1, 2, 3, 1500} {
		content := nestedToolResults(depth, strings.Repeat("safe text ", 10000))
		c := &Capture{}
		c.Input([]byte(`{"messages":[{"role":"user","content":` + content + `}]}`))
		start := time.Now()
		d := c.decode()
		if time.Since(start) > time.Second {
			t.Fatal("nested decoding exceeded bounded work", depth)
		}
		if len(d.Input) != 1 || len(d.Input[0].Blocks) != 1 {
			t.Fatal("missing tool result", depth, d)
		}
		text := d.Input[0].Blocks[0].Text
		if depth <= 3 && !strings.Contains(text, "safe text") || depth > rawMaxDepth && !strings.Contains(text, "nested content omitted") {
			t.Fatal("nested rendering", depth, text)
		}
	}
}

func TestHistoricalTracesUseCurrentSanitizationOnRead(t *testing.T) {
	r, _ := newRedactor([]string{`ORG-[0-9]{6}`})
	s := &Service{redactor: r}
	raw, err := s.SanitizeDetail([]byte(`{"v":2,"input":[],"output":[{"role":"assistant","blocks":[{"type":"text","text":"Bearer historical-secret ORG-123456"}]}],"parameters":{"seed":1234567890123456789,"secret":"historical-parameter"},"events":[{"type":"text","text":"Bearer "},{"type":"text","text":"historical-secret"}],"frames":[{"ms":1,"data":{"choices":[{"delta":{"content":"historical-fragment"}}]}}],"warnings":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"historical-secret", "ORG-123456", "historical-parameter", "historical-fragment"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("historical trace exposed old content", secret)
		}
	}
	if !strings.Contains(string(raw), `"seed":1234567890123456789`) || !strings.Contains(string(raw), `"v":4`) {
		t.Fatal("historical normalization lost precision or version", string(raw))
	}
}

func BenchmarkNestedToolResults(b *testing.B) {
	for _, size := range []int{64 << 10, 512 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			data := []byte(`{"messages":[{"role":"user","content":` + nestedToolResults(1500, strings.Repeat("x ", size/2)) + `}]}`)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				c := &Capture{}
				c.Input(data)
				c.decode()
			}
		})
	}
}
