package usage

import (
	"bytes"
	"strings"
	"testing"
)

func TestResponsesJSONUsage(t *testing.T) {
	wire := `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1000,"output_tokens":7,"input_tokens_details":{"cached_tokens":900},"output_tokens_details":{"reasoning_tokens":5},"total_tokens":1007}}`
	for split := 0; split <= len(wire); split++ {
		j := &JSON{Protocol: "openai-responses"}
		j.Write([]byte(wire[:split]))
		j.Write([]byte(wire[split:]))
		if j.Totals.InputTokens != 1000 || j.Totals.Output() != 7 || j.Status != "completed" {
			t.Fatalf("split %d: %+v status=%s", split, j.Totals, j.Status)
		}
	}
}

func TestResponsesSSEUsageAndTermination(t *testing.T) {
	deltas := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"think\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\"}\n\n"
	for _, tc := range []struct {
		kind             string
		complete, failed bool
	}{
		{"response.completed", true, false}, {"response.incomplete", true, false},
		{"response.failed", true, true}, {"error", true, true}, {"", false, false},
	} {
		wire := ": heartbeat\n\n" + deltas
		if tc.kind != "" {
			wire += "event: " + tc.kind + "\ndata: {\"type\":\"" + tc.kind + "\",\"response\":{\"usage\":{\"input_tokens\":1000,\"output_tokens\":7}}}\n\n"
		}
		for split := 0; split <= len(wire); split++ {
			var out bytes.Buffer
			s := NewStream("openai-responses", true, func(p []byte) error { _, err := out.Write(p); return err })
			_ = s.Write([]byte(wire[:split]))
			_ = s.Write([]byte(wire[split:]))
			_ = s.Finish()
			if out.String() != wire || s.Totals.Complete != tc.complete || s.Totals.StreamError != tc.failed || s.Totals.ContentEvents != 3 {
				t.Fatalf("%s split %d: %+v changed=%v", tc.kind, split, s.Totals, out.String() != wire)
			}
			if strings.HasPrefix(tc.kind, "response.") && (s.Totals.InputTokens != 1000 || s.Totals.Output() != 7) {
				t.Fatal("lost authoritative usage", s.Totals)
			}
		}
	}
}

func TestResponsesLargeTerminalEvent(t *testing.T) {
	// The terminal response includes the full answer. Its usage and type may
	// follow megabytes of output; inspection must stay bounded and complete.
	wire := "data: {\"response\":{\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"" + strings.Repeat("x", 2*maxEvent) + "\"}]}],\"usage\":{\"input_tokens\":123,\"output_tokens\":456}},\"type\":\"response.completed\"}\n\n"
	for _, size := range []int{7, 4096, len(wire)} {
		var out bytes.Buffer
		s := NewStream("openai-responses", false, func(p []byte) error { _, err := out.Write(p); return err })
		for start := 0; start < len(wire); start += size {
			_ = s.Write([]byte(wire[start:min(start+size, len(wire))]))
		}
		_ = s.Finish()
		if out.String() != wire || !s.Totals.Complete || s.Totals.InputTokens != 123 || s.Totals.Output() != 456 {
			t.Fatalf("chunk %d: %+v", size, s.Totals)
		}
	}
}

func TestResponsesSSEFraming(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		wire := "\xef\xbb\xbfdata: {\"type\":\"response.completed\"," + ending + "data: \"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}" + ending + ending
		for split := 0; split <= len(wire); split++ {
			var out bytes.Buffer
			s := NewStream("openai-responses", false, func(p []byte) error { _, err := out.Write(p); return err })
			_ = s.Write([]byte(wire[:split]))
			_ = s.Write([]byte(wire[split:]))
			_ = s.Finish()
			if out.String() != wire || !s.Totals.Complete || s.Totals.InputTokens != 2 || s.Totals.Output() != 3 {
				t.Fatalf("ending=%q split=%d %+v", ending, split, s.Totals)
			}
		}
	}
}
