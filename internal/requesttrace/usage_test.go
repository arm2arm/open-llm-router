package requesttrace

import (
	"fmt"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/usage"
)

func TestHiddenProviderUsageSurvivesOutputCaptureLimit(t *testing.T) {
	c := &Capture{record: usage.Record{IsStream: true}}
	var client strings.Builder
	stream := usage.NewStream("openai", true, func(p []byte) error {
		c.Write(p)
		_, err := client.Write(p)
		return err
	})
	stream.UsageObserver = c.Usage
	for i := range 3000 {
		event := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"token %d \"}}]}\n\n", i)
		if err := stream.Write([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"late-tool\",\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3000,\"prompt_tokens_details\":{\"cached_tokens\":7},\"completion_tokens_details\":{\"reasoning_tokens\":9}}}\n\ndata: [DONE]\n\n")); err != nil {
		t.Fatal(err)
	}
	d := c.decode()
	if !d.OutputTruncated || d.Summary.ToolCalls != nil || d.Summary.FinishReason != "" || len(d.Result.FinishReasons) != 0 {
		t.Fatal("limited output reported a factual complete result", d.Summary)
	}
	if strings.Contains(client.String(), `"usage"`) {
		t.Fatal("client-visible usage contract changed")
	}
	if d.Result.Usage["completion_tokens"] != 3000 || d.Summary.CachedTokens != 7 || d.Summary.ReasoningTokens != 9 {
		t.Fatal("hidden terminal usage lost", d.Result.Usage, d.Summary)
	}
}

func TestUsageObservationKeepsAnthropicAndResponsesMetadata(t *testing.T) {
	for _, protocol := range []string{"anthropic", usage.Responses} {
		t.Run(protocol, func(t *testing.T) {
			c := &Capture{operation: "messages", record: usage.Record{IsStream: true}}
			stream := usage.NewStream(protocol, false, func(p []byte) error { c.Write(p); return nil })
			stream.UsageObserver = c.Usage
			data := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":4,\"cache_read_input_tokens\":3}}}\n\nevent: message_delta\ndata: {\"usage\":{\"output_tokens\":7}}\n\nevent: message_stop\ndata: {}\n\n"
			if protocol == usage.Responses {
				c.operation = "responses"
				data = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":3},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n"
			}
			// Force metadata to cross arbitrary wire boundaries and survive an
			// already exhausted content budget.
			c.outputStopped, c.truncated = true, true
			for _, b := range []byte(data) {
				if err := stream.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			d := c.decode()
			if d.Result.Usage["input_tokens"] != 4 || d.Result.Usage["output_tokens"] != 7 || d.Summary.CachedTokens != 3 {
				t.Fatal("independent usage lost", d.Result.Usage, d.Summary)
			}
			if protocol == usage.Responses && d.Summary.ReasoningTokens != 2 {
				t.Fatal("Responses reasoning metadata lost")
			}
		})
	}
}

func TestProviderUsageCopyHasIndependentBounds(t *testing.T) {
	c := &Capture{outputStopped: true}
	for i := range 100 {
		c.Usage([]byte(fmt.Sprintf(`{"output_tokens":%d,"extension":"%s"}`, i, strings.Repeat("x", 20000))))
	}
	c.Usage([]byte(strings.Repeat("x", MaxProviderUsage+1)))
	if c.providerUsageBytes > MaxProviderUsage || len(c.providerUsage) > 4 || c.decode().Result.Usage["output_tokens"] != 99 {
		t.Fatal("usage copy unbounded or lost latest totals", c.providerUsageBytes)
	}
}

func TestInputOnlyLimitKeepsKnownZeroOutputCount(t *testing.T) {
	c := &Capture{jsonResponse: true}
	c.Input([]byte(strings.Repeat("x", MaxInput+1)))
	c.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"complete"},"finish_reason":"stop"}]}`))
	d := c.decode()
	if !d.Truncated || d.OutputTruncated || d.Summary.ToolCalls == nil || *d.Summary.ToolCalls != 0 || d.Summary.FinishReason != "stop" {
		t.Fatal("input limit erased complete output facts", d.Summary)
	}
}
