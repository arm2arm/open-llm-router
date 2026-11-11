package usage

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/arm2arm/open-llm-router/internal/config"
)

const openAIContentEvent = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":null}\n\n"
const openAIUsageEvent = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\n"

func observeStream(t *testing.T, protocol string, hide bool, parts ...[]byte) ([]byte, Totals) {
	t.Helper()
	var output bytes.Buffer
	s := NewStream(protocol, hide, func(p []byte) error { _, err := output.Write(p); return err })
	for _, part := range parts {
		if err := s.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes(), s.Totals
}

func TestStreamEverySplit(t *testing.T) {
	openAI := ": heartbeat\n\nid: 7\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
		openAIContentEvent + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		openAIUsageEvent + "data: [DONE]\n\n"
	anthropic := "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"signature_delta\",\"signature\":\"opaque\"}}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_delta\ndata: {\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: future_event\ndata: unknown\n\nevent: message_stop\ndata: {}\n\n"
	for _, protocol := range []string{config.ProtocolOpenAI, config.ProtocolAnthropic} {
		input := openAI
		if protocol == config.ProtocolAnthropic {
			input = anthropic
		}
		for _, eol := range []string{"\n", "\r\n", "\r"} {
			for _, hide := range []bool{false, true} {
				wire := []byte(strings.ReplaceAll(input, "\n", eol))
				want := wire
				if hide && protocol == config.ProtocolOpenAI {
					want = bytes.Replace(wire, []byte(strings.ReplaceAll(openAIUsageEvent, "\n", eol)), nil, 1)
				}
				for split := 0; split <= len(wire); split++ {
					output, totals := observeStream(t, protocol, hide, wire[:split], wire[split:])
					if !bytes.Equal(output, want) || totals.InputTokens != 12 || totals.OutputTokens != 7 || !totals.HasInput || !totals.HasOutput || totals.ContentEvents != 1 || !totals.Complete {
						t.Fatalf("%s eol=%q hide=%t split=%d: totals=%+v output=%q", protocol, eol, hide, split, totals, output)
					}
				}
				parts := make([][]byte, len(wire))
				for i := range wire {
					parts[i] = wire[i : i+1]
				}
				output, totals := observeStream(t, protocol, hide, parts...)
				if !bytes.Equal(output, want) || totals.OutputTokens != 7 || !totals.Complete {
					t.Fatalf("byte-wise %s: %+v", protocol, totals)
				}
			}
		}
	}
}

func TestStreamMultilineUnknownAndIncomplete(t *testing.T) {
	wire := []byte("\xef\xbb\xbfretry: 1000\r\n: comment\r\ndata: {\"choices\": [ ],\r\ndata: \"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\r\n\r\n" +
		"event: future\ndata: {\"text\":\"kept\"}\n\n" + "data: {\"usage\":{\"prompt_tokens\":999}}")
	output, totals := observeStream(t, config.ProtocolOpenAI, false, wire)
	if !bytes.Equal(output, wire) || totals.InputTokens != 12 || totals.OutputTokens != 7 || totals.Complete {
		t.Fatalf("%q %+v", output, totals)
	}
}

func TestStreamOversizedEventPassesThrough(t *testing.T) {
	wire := []byte("data: " + strings.Repeat("x", maxEvent+17) + "\n\n" + openAIUsageEvent)
	output, totals := observeStream(t, config.ProtocolOpenAI, false, wire[:400], wire[400:])
	if !bytes.Equal(output, wire) || totals.OutputTokens != 7 {
		t.Fatal("oversized event changed bytes or hid the next event", totals)
	}
}

func TestStreamFallbackCountsContentOnly(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" + openAIContentEvent +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"text\":\"completion\"}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	_, totals := observeStream(t, config.ProtocolOpenAI, false, []byte(wire))
	if totals.HasOutput || totals.Output() != 3 || totals.Complete {
		t.Fatal(totals)
	}
	_, totals = observeStream(t, config.ProtocolAnthropic, false, []byte("event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n"+
		"event: content_block_delta\ndata: {\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"word\"}}\n\n"))
	if !totals.HasInput || totals.InputTokens != 12 || totals.HasOutput || totals.Output() != 1 {
		t.Fatal(totals)
	}
}

func TestStreamDoesNotDropUsageOnContentOrInvalidCounts(t *testing.T) {
	for _, event := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n",
		"data: {\"choices\":[],\"usage\":null}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-1}}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":\"bad\"}}\n\n",
	} {
		output, _ := observeStream(t, config.ProtocolOpenAI, true, []byte(event))
		if string(output) != event {
			t.Fatal("dropped a non-usage-only event")
		}
	}
}

func TestStreamPropagatesWriteFailure(t *testing.T) {
	failure := errors.New("disconnected")
	s := NewStream(config.ProtocolOpenAI, false, func([]byte) error { return failure })
	if !errors.Is(s.Write([]byte(openAIContentEvent)), failure) {
		t.Fatal("swallowed a write failure")
	}
}

func TestContentEventsAllocateNoJSONObjects(t *testing.T) {
	s := NewStream(config.ProtocolOpenAI, false, func([]byte) error { return nil })
	p := []byte(openAIContentEvent)
	if allocs := testing.AllocsPerRun(100, func() { _ = s.Write(p) }); allocs != 0 {
		t.Fatalf("ordinary content event allocated %v objects", allocs)
	}
}

func FuzzStreamPreservesBytes(f *testing.F) {
	for _, seed := range []string{openAIContentEvent + openAIUsageEvent + "data: [DONE]\n\n", "event: ping\r\ndata: {}\r\n\r\n", "data: x\r\rdata: partial", "\x00\xff\n\n"} {
		f.Add([]byte(seed), uint16(7))
	}
	f.Fuzz(func(t *testing.T, wire []byte, width uint16) {
		if len(wire) > 2*maxEvent {
			t.Skip()
		}
		for _, protocol := range []string{config.ProtocolOpenAI, config.ProtocolAnthropic} {
			whole, expected := observeStream(t, protocol, false, wire)
			var parts [][]byte
			step := int(width)%257 + 1
			for start := 0; start < len(wire); start += step {
				parts = append(parts, wire[start:min(start+step, len(wire))])
			}
			fragmented, actual := observeStream(t, protocol, false, parts...)
			if !bytes.Equal(whole, wire) || !bytes.Equal(fragmented, wire) || expected != actual {
				t.Fatal("fragmentation changed bytes or accounting")
			}
		}
	})
}
