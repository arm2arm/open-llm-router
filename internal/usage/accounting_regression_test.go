package usage

import (
	"bytes"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestIncludeUsageRejectsAmbiguousKeys(t *testing.T) {
	for _, body := range []string{
		`{"stream_options":{"include_usage":true,"Include_Usage":false}}`,
		`{"stream_options":{"Include_Usage":false}}`,
		`{"Stream_Options":{"include_usage":false}}`,
		`{"stream_options":{"include_usage":true},"Stream_Options":{"include_usage":false}}`,
		`{"stream_options":{"include_usage":true},"stream_options":{"include_usage":false}}`,
		`{"stream_options":{"include_usage":true,"include_usage":false}}`,
		`{"stream_options":{"include_usage":false,"include_usage":true}}`,
		`{"stream_options":{"include_usage":true,"include_\u0075sage":false}}`,
	} {
		if _, _, err := IncludeUsage([]byte(body)); err == nil {
			t.Errorf("ambiguous accounting keys accepted: %s", body)
		}
	}
}

func TestAnthropicCachedAndFinalInputUsage(t *testing.T) {
	j := JSON{Protocol: config.ProtocolAnthropic}
	j.Write([]byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":3000,"cache_creation_input_tokens":500,"output_tokens":7}}`))
	if j.Totals.InputTokens != 3510 || j.Totals.Output() != 7 {
		t.Fatal("cached JSON usage", j.Totals)
	}
	for _, tc := range []struct {
		delta string
		input int64
	}{
		{`{"input_tokens":40,"cache_read_input_tokens":3000,"output_tokens":7}`, 3040},
		{`{"output_tokens":7}`, 12},
		{`{"input_tokens":0,"cache_read_input_tokens":3000,"cache_creation_input_tokens":500,"output_tokens":7}`, 3500},
	} {
		wire := []byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12}}}\n\nevent: message_delta\ndata: {\"usage\":" + tc.delta + "}\n\nevent: message_stop\ndata: {}\n\n")
		for split := 0; split <= len(wire); split++ {
			out, totals := observeStream(t, config.ProtocolAnthropic, false, wire[:split], wire[split:])
			if !bytes.Equal(out, wire) || totals.InputTokens != tc.input || totals.Output() != 7 || !totals.Complete {
				t.Fatalf("split=%d: %+v", split, totals)
			}
		}
	}
}

func TestAnthropicInputPartsMergeWithoutOverflow(t *testing.T) {
	totals := Totals{}
	if !totals.readUsage([]byte(`{"input_tokens":12,"cache_read_input_tokens":3000,"cache_creation_input_tokens":500}`), config.ProtocolAnthropic, true, true) || totals.InputTokens != 3512 {
		t.Fatal(totals)
	}
	totals.readUsage([]byte(`{"input_tokens":40}`), config.ProtocolAnthropic, true, true)
	if totals.InputTokens != 3540 {
		t.Fatal("partial final usage erased cached tokens", totals)
	}
	for _, raw := range []string{`{"input_tokens":-1,"output_tokens":9}`, `{"cache_read_input_tokens":-1,"output_tokens":9}`, `{"input_tokens":9223372036854775807,"cache_creation_input_tokens":1,"output_tokens":9}`} {
		totals.readUsage([]byte(raw), config.ProtocolAnthropic, true, true)
		if totals.InputTokens != 3540 || totals.OutputTokens != 9 {
			t.Fatal("invalid input sum replaced valid usage", totals)
		}
	}
	openai := JSON{Protocol: config.ProtocolOpenAI}
	openai.Write([]byte(`{"usage":{"prompt_tokens":3510,"completion_tokens":7,"cache_read_input_tokens":3000}}`))
	if openai.Totals.InputTokens != 3510 {
		t.Fatal("OpenAI cache counted twice", openai.Totals)
	}
}
