package usage

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

const timingExtra = `,"olr_observed_output_tokens_per_second":3.5,"olr_observed_elapsed_seconds":2`

func timed(t *testing.T, protocol string, sse bool, emit func([]byte) error) *Timing {
	t.Helper()
	start := time.Unix(1, 0)
	value := NewTiming(protocol, sse, start, emit)
	value.now = func() time.Time { return start.Add(2 * time.Second) }
	return value
}

func TestTimingPreservesProviderJSONAndSSEEverySplit(t *testing.T) {
	for _, protocol := range []string{"openai", Responses} {
		counts := `"prompt_tokens":1000,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":5},"prompt_tokens_details":{"cached_tokens":900}`
		if protocol == Responses {
			counts = `"input_tokens":1000,"output_tokens":7,"output_tokens_details":{"reasoning_tokens":5},"input_tokens_details":{"cached_tokens":900}`
		}
		body := `{"output":[{"type":"function_call","arguments":"{\"question\":\"usage\"}"}],"usage":{` + counts + `,"provider_timing":{"decode_ms":42},"response_token/s":99,"unknown":123456789012345678901234567890}}`
		for _, sse := range []bool{false, true} {
			for _, eol := range []string{"\n", "\r\n", "\r"} {
				wire := body
				if sse {
					if protocol == Responses {
						wire = `{"type":"response.completed","response":` + body + `}`
					}
					// Split data fields outside the usage object, and retain
					// comments, a BOM, unknown SSE fields and exact line endings.
					wire = ": heartbeat" + eol + eol + "\xef\xbb\xbfdata: " + strings.Replace(wire, `,"usage":`, ","+eol+`data: "usage":`, 1) + eol + "id: original" + eol + eol
					if protocol == "openai" {
						wire += "data: [DONE]" + eol + eol
					}
				}
				for split := 0; split <= len(wire); split++ {
					var out bytes.Buffer
					value := timed(t, protocol, sse, func(p []byte) error { _, err := out.Write(p); return err })
					if err := value.Write([]byte(wire[:split])); err != nil {
						t.Fatal(err)
					}
					if err := value.Write([]byte(wire[split:])); err != nil {
						t.Fatal(err)
					}
					got := out.String()
					if strings.Count(got, timingExtra) != 1 || strings.Replace(got, timingExtra, "", 1) != wire {
						t.Fatalf("protocol=%s sse=%v eol=%q split=%d: %s", protocol, sse, eol, split, got)
					}
				}
			}
		}
	}
}

func TestTimingMissingInvalidOversizedAndCollidingUsage(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"prompt_tokens":9}`, `{"completion_tokens":null}`,
		`{"completion_tokens":-1}`, `{"completion_tokens":2.5}`, `{"completion_tokens":true}`,
		`{"completion_tokens":9223372036854775808}`,
		`{"completion_tokens":7,"olr_observed_output_tokens_per_second":null}`,
		`{"completion_tokens":7,"olr_observed_elapsed_seconds":12}`,
		`{"completion_tokens":7,"padding":"` + strings.Repeat("x", maxUsage) + `"}`,
	} {
		wire := `{"usage":` + raw + `}`
		var out bytes.Buffer
		value := timed(t, "openai", false, func(p []byte) error { _, err := out.Write(p); return err })
		for i := 0; i < len(wire); i += 7 {
			if err := value.Write([]byte(wire[i:min(i+7, len(wire))])); err != nil {
				t.Fatal(err)
			}
		}
		if out.String() != wire || cap(value.json.value) > 2*maxUsage {
			t.Fatal("changed unsupported usage or exceeded bounded capture")
		}
	}
}

func TestTimingSSEUsageAcrossDataFields(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n", "\r"} {
		wire := `data: {"usage":{"prompt_tokens":2,` + eol + `data: "completion_tokens":7}}` + eol + eol +
			`data: {"usage":{"completion_tokens":7}}` + eol + eol + "data: [DONE]" + eol + eol
		var out bytes.Buffer
		value := timed(t, "openai", true, func(p []byte) error { _, err := out.Write(p); return err })
		for i := range len(wire) {
			if err := value.Write([]byte(wire[i : i+1])); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Count(out.String(), timingExtra) != 2 || strings.ReplaceAll(out.String(), timingExtra, "") != wire {
			t.Fatal("changed multi-line data or leaked state across events", out.String())
		}
	}
}

func TestTimingAuthoritativeZeroAndNoCompletionClaim(t *testing.T) {
	for _, wire := range []string{
		`data: {"choices":[],"usage":{"completion_tokens":0}}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{"}}]}}]}` + "\n\n",
		`data: {"usage":{"completion_tokens":7`, // disconnected before usage is complete
	} {
		var out bytes.Buffer
		value := timed(t, "openai", true, func(p []byte) error { _, err := out.Write(p); return err })
		if err := value.Write([]byte(wire)); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if strings.Contains(wire, `"completion_tokens":0`) {
			if !strings.Contains(got, `"olr_observed_output_tokens_per_second":0`) {
				t.Fatal("lost authoritative zero", got)
			}
		} else if got != wire {
			t.Fatal("invented timing for missing/incomplete usage", got)
		}
		if strings.Contains(got, "[DONE]") {
			t.Fatal("synthesized a completion marker")
		}
	}
}

func TestTimingLargeResponsesEventIsIncremental(t *testing.T) {
	for _, usageFirst := range []bool{false, true} {
		usage := `"usage":{"input_tokens":1000,"output_tokens":7}`
		output := `"output":[{"type":"message","content":[{"text":"` + strings.Repeat("x", 3*maxEvent) + `"}]}]`
		members := output + "," + usage
		if usageFirst {
			members = usage + "," + output
		}
		wire := `data: {"response":{` + members + `},"type":"response.completed"}` + "\n\n"
		for _, size := range []int{7, 4096, len(wire)} {
			var out bytes.Buffer
			value := timed(t, Responses, true, func(p []byte) error { _, err := out.Write(p); return err })
			for i := 0; i < len(wire); i += size {
				if err := value.Write([]byte(wire[i:min(i+size, len(wire))])); err != nil {
					t.Fatal(err)
				}
				if out.Len() < min(i+size, len(wire)) || cap(value.json.value) > 2*maxUsage {
					t.Fatal("buffered the terminal event instead of forwarding it incrementally")
				}
			}
			if strings.Replace(out.String(), timingExtra, "", 1) != wire {
				t.Fatal("changed a large response")
			}
		}
	}
}

func TestTimingWriteErrorAndNonpositiveClock(t *testing.T) {
	failure := errors.New("client disconnected")
	value := timed(t, "openai", false, func([]byte) error { return failure })
	if err := value.Write([]byte(`{"usage":{"completion_tokens":7}}`)); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	var out bytes.Buffer
	value = timed(t, "openai", false, func(p []byte) error { _, err := out.Write(p); return err })
	value.now = func() time.Time { return value.began }
	wire := `{"usage":{"completion_tokens":7}}`
	if err := value.Write([]byte(wire)); err != nil || out.String() != wire {
		t.Fatal("reported an invalid rate", err, out.String())
	}
}

func FuzzTimingPreservesProviderBytes(f *testing.F) {
	f.Add([]byte(`{"usage":{"completion_tokens":7}}`), byte(0), byte(1))
	f.Add([]byte("data: {\"usage\":{\"completion_tokens\":7}}\r\n\r\ndata: [DONE]\r\n\r\n"), byte(1), byte(7))
	f.Add([]byte("data: {\"response\":{\"usage\":{\"output_tokens\":7}},\"type\":\"response.completed\"}\n\n"), byte(3), byte(1))
	extra := regexp.MustCompile(`,"olr_observed_output_tokens_per_second":[0-9.e+\-]+,"olr_observed_elapsed_seconds":[0-9.e+\-]+`)
	f.Fuzz(func(t *testing.T, wire []byte, mode, chunk byte) {
		if bytes.Contains(wire, []byte(`"olr_observed_output_tokens_per_second"`)) {
			t.Skip("provider-owned extension")
		}
		protocol := "openai"
		if mode&2 != 0 {
			protocol = Responses
		}
		var out bytes.Buffer
		value := timed(t, protocol, mode&1 != 0, func(p []byte) error { _, err := out.Write(p); return err })
		for i := 0; i < len(wire); i += int(chunk) + 1 {
			if err := value.Write(wire[i:min(i+int(chunk)+1, len(wire))]); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(extra.ReplaceAll(out.Bytes(), nil), wire) || cap(value.json.value) > 2*maxUsage {
			t.Fatal("changed provider bytes or exceeded bounded usage capture")
		}
	})
}
