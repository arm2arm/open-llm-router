package usage

import (
	"bytes"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func TestJSONUsageEverySplit(t *testing.T) {
	for _, tc := range []struct{ protocol, wire string }{
		{config.ProtocolOpenAI, `{"choices":[{"message":{"content":"a string with \\\"usage\\\" and {}"},"usage":{"prompt_tokens":999}}],"usage":{"prompt_tokens":12,"completion_tokens":7},"extra":[1,{"x":"}"}]}`},
		{config.ProtocolAnthropic, `{"usage":{"input_tokens":12,"output_tokens":7},"content":[{"type":"text","text":"hello"}]}`},
		{config.ProtocolOpenAI, `{"us\u0061ge":{"prompt_tokens":12,"completion_tokens":7}}`},
	} {
		for split := 0; split <= len(tc.wire); split++ {
			j := JSON{Protocol: tc.protocol}
			j.Write([]byte(tc.wire[:split]))
			j.Write([]byte(tc.wire[split:]))
			if j.Totals.InputTokens != 12 || j.Totals.OutputTokens != 7 || !j.Totals.HasOutput || !j.Totals.HasInput {
				t.Fatalf("%s split=%d: %+v", tc.protocol, split, j.Totals)
			}
		}
	}
}

func TestJSONSkipsContentWithoutBuffering(t *testing.T) {
	j := JSON{Protocol: config.ProtocolOpenAI}
	j.Write([]byte(`{"content":"`))
	block := bytes.Repeat([]byte("x"), 64<<10)
	for range 100 {
		j.Write(block)
	}
	j.Write([]byte(`","usage":{"prompt_tokens":12}}`))
	if j.Totals.InputTokens != 12 || len(j.value) != 0 || cap(j.value) > maxUsage || cap(j.keyBytes) > 64 {
		t.Fatal("retained response content", j.Totals)
	}
}

func TestJSONInvalidOrNestedUsageIgnored(t *testing.T) {
	for _, wire := range []string{
		`{"nested":{"usage":{"prompt_tokens":99}}}`,
		`{"content":"usage: {prompt_tokens: 99}"}`,
		`{"usage":{"prompt_tokens":-1,"completion_tokens":-1}}`,
		`{"usage":{"prompt_tokens":"99"}}`,
		`{"usage":null}`,
		`{"usage":{"prompt_tokens":99`,
		`{"usage":{"padding":"` + strings.Repeat("x", maxUsage) + `","prompt_tokens":99}}`,
	} {
		j := JSON{Protocol: config.ProtocolOpenAI}
		j.Write([]byte(wire))
		if j.Totals.HasInput || j.Totals.HasOutput {
			t.Fatal("read invalid or nested usage", j.Totals)
		}
	}
}

func TestIncludeUsagePreservesClientOptions(t *testing.T) {
	for _, options := range []string{"", `,"stream_options":null`, `,"stream_options":{"include_usage":false,"future":123456789012345678901234567890}`} {
		body := []byte(`{"model":"m","unknown":123456789012345678901234567890` + options + `}`)
		updated, asked, err := IncludeUsage(body)
		if err != nil || asked || !bytes.Contains(updated, []byte(`"include_usage":true`)) || !bytes.Contains(updated, []byte(`"unknown":123456789012345678901234567890`)) ||
			(strings.Contains(options, "future") && !bytes.Contains(updated, []byte(`"future":123456789012345678901234567890`))) {
			t.Fatalf("%s asked=%t err=%v", updated, asked, err)
		}
	}
	body := []byte("{ \"stream_options\":{\"include_usage\":true}, \"model\":\"m\" }")
	updated, asked, err := IncludeUsage(body)
	if err != nil || !asked || !bytes.Equal(updated, body) {
		t.Fatal("changed a client-requested usage option")
	}
	for _, body := range []string{`null`, `{`, `{"stream_options":[]}`, `{"stream_options":"bad"}`, `{"stream_options":{"include_usage":"yes"}}`} {
		if _, _, err := IncludeUsage([]byte(body)); err == nil {
			t.Fatal("accepted invalid options", body)
		}
	}
}
