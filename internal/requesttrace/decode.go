package requesttrace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Block struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Name      string `json:"name,omitempty"`
	ID        string `json:"id,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
type Message struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}
type Event struct {
	MS   int64  `json:"ms"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
type Detail struct {
	Operation  string         `json:"operation"`
	Input      []Message      `json:"input"`
	Output     []Message      `json:"output"`
	Events     []Event        `json:"events"`
	Parameters map[string]any `json:"parameters"`
	Truncated  bool           `json:"truncated"`
	Warnings   []string       `json:"warnings"`
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any            { a, _ := v.([]any); return a }
func str(v any) string            { s, _ := v.(string); return s }
func compact(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Only known content fields cross the storage boundary. Image/document/audio
// payloads, URLs (often signed), signatures, vectors and arbitrary provider
// metadata never enter the trace. Text and tool arguments can contain personal
// data: they are deliberately controlled by the user's capture setting.
func blocks(v any) []Block {
	if s, ok := v.(string); ok {
		return []Block{{Type: "text", Text: s}}
	}
	out := []Block{}
	for _, item := range list(v) {
		m := object(item)
		switch str(m["type"]) {
		case "text", "input_text", "output_text":
			out = append(out, Block{Type: "text", Text: str(m["text"])})
		case "thinking":
			out = append(out, Block{Type: "reasoning", Text: str(m["thinking"])})
		case "redacted_thinking":
			out = append(out, Block{Type: "reasoning", Text: "[Provider redacted reasoning]"})
		case "tool_use":
			out = append(out, Block{Type: "tool_call", ID: str(m["id"]), Name: str(m["name"]), Arguments: compact(m["input"])})
		case "tool_result":
			out = append(out, Block{Type: "tool_result", ID: str(m["tool_use_id"]), Text: contentText(m["content"])})
		case "image", "image_url", "input_image":
			out = append(out, Block{Type: "image", Text: "Image payload omitted"})
		case "document", "file", "input_file":
			out = append(out, Block{Type: "document", Text: "Document payload omitted"})
		case "input_audio", "audio":
			out = append(out, Block{Type: "audio", Text: "Audio payload omitted"})
		}
	}
	return out
}
func contentText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, b := range blocks(v) {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
func message(m map[string]any) Message {
	msg := Message{Role: str(m["role"]), Blocks: blocks(m["content"])}
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	switch msg.Role {
	case "system", "developer", "user", "assistant", "tool", "function":
	default:
		msg.Role = "unknown"
	}
	if s := str(m["reasoning_content"]); s != "" {
		msg.Blocks = append([]Block{{Type: "reasoning", Text: s}}, msg.Blocks...)
	}
	if s := str(m["reasoning"]); s != "" {
		msg.Blocks = append([]Block{{Type: "reasoning", Text: s}}, msg.Blocks...)
	}
	if s := str(m["refusal"]); s != "" {
		msg.Blocks = append(msg.Blocks, Block{Type: "refusal", Text: s})
	}
	for _, v := range list(m["tool_calls"]) {
		call := object(v)
		f := object(call["function"])
		msg.Blocks = append(msg.Blocks, Block{Type: "tool_call", ID: str(call["id"]), Name: str(f["name"]), Arguments: compact(f["arguments"])})
	}
	if f := object(m["function_call"]); f != nil {
		msg.Blocks = append(msg.Blocks, Block{Type: "tool_call", Name: str(f["name"]), Arguments: compact(f["arguments"])})
	}
	if msg.Role == "tool" || msg.Role == "function" {
		for i := range msg.Blocks {
			msg.Blocks[i].Type = "tool_result"
			msg.Blocks[i].ID = str(m["tool_call_id"])
			msg.Blocks[i].Name = str(m["name"])
		}
	}
	return msg
}

func (c *Capture) decode() Detail {
	d := Detail{Operation: c.operation, Input: []Message{}, Output: []Message{}, Events: []Event{}, Parameters: map[string]any{}, Warnings: []string{}, Truncated: c.truncated}
	var input map[string]any
	if len(c.input) > 0 && json.Unmarshal(c.input, &input) == nil {
		if sys, ok := input["system"]; ok {
			d.Input = append(d.Input, Message{Role: "system", Blocks: blocks(sys)})
		}
		for _, v := range list(input["messages"]) {
			d.Input = append(d.Input, message(object(v)))
		}
		for _, key := range []string{"prompt", "input"} {
			if v, ok := input[key]; ok {
				text := str(v)
				if text == "" {
					text = "[Non-text input omitted]"
				}
				d.Input = append(d.Input, Message{Role: "user", Blocks: []Block{{Type: "text", Text: text}}})
			}
		}
		if model := str(input["model"]); model != "" {
			d.Parameters["model"] = model
		}
		for _, key := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "seed", "stream"} {
			if v, ok := input[key]; ok {
				switch v.(type) {
				case float64, bool:
					d.Parameters[key] = v
				}
			}
		}
		// Tool definitions can be large. Store names, not arbitrary schemas or metadata.
		var names []string
		for _, v := range list(input["tools"]) {
			m := object(v)
			name := str(m["name"])
			if f := object(m["function"]); f != nil {
				name = str(f["name"])
			}
			if name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			d.Parameters["tools"] = names
		}
	} else if len(c.input) == 0 {
		d.Warnings = append(d.Warnings, "Input unavailable or above the 128 KiB capture limit.")
	}
	if c.record.IsStream {
		c.decodeStream(&d)
	} else if c.jsonResponse {
		var m map[string]any
		if json.Unmarshal(c.output, &m) == nil {
			for _, v := range list(m["choices"]) {
				choice := object(v)
				if msg := object(choice["message"]); msg != nil {
					d.Output = append(d.Output, message(msg))
				} else if s := str(choice["text"]); s != "" {
					d.Output = append(d.Output, Message{Role: "assistant", Blocks: []Block{{Type: "text", Text: s}}})
				}
			}
			if _, ok := m["content"]; ok {
				d.Output = append(d.Output, message(m))
			}
			if m["error"] != nil {
				d.Events = append(d.Events, Event{MS: c.record.LatencyMS, Type: "error", Text: providerError(m["error"])})
			}
			if _, ok := m["data"]; ok && c.operation == "embeddings" {
				d.Warnings = append(d.Warnings, "Embedding vectors are omitted.")
			}
		} else if len(c.output) > 0 {
			d.Warnings = append(d.Warnings, "Incomplete or invalid JSON response; content could not be reconstructed.")
		}
	} else if len(c.output) > 0 {
		d.Warnings = append(d.Warnings, "Unsupported response content type; body omitted.")
	}
	if c.record.Status == "aborted" {
		d.Warnings = append(d.Warnings, "Response interrupted. Only received content and complete stream events are shown.")
	}
	if d.Truncated {
		d.Warnings = append(d.Warnings, "Capture limit reached. The response delivered to your client was not truncated by tracing.")
	}
	return d
}

func (c *Capture) decodeStream(d *Detail) {
	state := map[string]*Block{}
	order := []string{}
	roles := map[string]string{}
	add := func(choice, key, kind, text, name, id, args string, ms int64) {
		full := choice + ":" + key
		b := state[full]
		if b == nil {
			b = &Block{Type: kind}
			state[full] = b
			order = append(order, full)
			roles[full] = choice
		}
		b.Text += text
		b.Name += name
		b.ID += id
		b.Arguments += args
		if len(d.Events) < MaxEvents {
			d.Events = append(d.Events, Event{MS: ms, Type: kind, Text: firstNonempty(text, name, args, id)})
		} else {
			d.Truncated = true
		}
	}
	consume := func(data []byte, ms int64) {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			if len(d.Events) < MaxEvents {
				d.Events = append(d.Events, Event{MS: ms, Type: "complete"})
			}
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			return
		}
		kind := str(m["type"])
		index := fmt.Sprint(m["index"])
		switch kind {
		case "content_block_start":
			b := object(m["content_block"])
			switch str(b["type"]) {
			case "tool_use":
				args := ""
				if in := object(b["input"]); len(in) > 0 {
					args = compact(in)
				}
				add("0", index, "tool_call", "", str(b["name"]), str(b["id"]), args, ms)
			case "text":
				add("0", index, "text", str(b["text"]), "", "", "", ms)
			case "thinking":
				add("0", index, "reasoning", str(b["thinking"]), "", "", "", ms)
			}
		case "content_block_delta":
			b := object(m["delta"])
			switch str(b["type"]) {
			case "text_delta":
				add("0", index, "text", str(b["text"]), "", "", "", ms)
			case "thinking_delta":
				add("0", index, "reasoning", str(b["thinking"]), "", "", "", ms)
			case "input_json_delta":
				add("0", index, "tool_call", "", "", "", str(b["partial_json"]), ms)
			}
		case "message_stop":
			if len(d.Events) < MaxEvents {
				d.Events = append(d.Events, Event{MS: ms, Type: "complete"})
			}
		}
		if kind == "error" || m["error"] != nil {
			if len(d.Events) < MaxEvents {
				d.Events = append(d.Events, Event{MS: ms, Type: "error", Text: providerError(m["error"])})
			}
		}
		for _, v := range list(m["choices"]) {
			choice := object(v)
			idx := fmt.Sprint(choice["index"])
			delta := object(choice["delta"])
			for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
				if text := str(delta[key]); text != "" {
					typ := "text"
					if strings.HasPrefix(key, "reasoning") {
						typ = "reasoning"
					}
					if key == "refusal" {
						typ = "refusal"
					}
					add(idx, typ, typ, text, "", "", "", ms)
				}
			}
			if text := str(choice["text"]); text != "" {
				add(idx, "text", "text", text, "", "", "", ms)
			}
			for _, v := range list(delta["tool_calls"]) {
				call := object(v)
				f := object(call["function"])
				add(idx, "tool-"+fmt.Sprint(call["index"]), "tool_call", "", str(f["name"]), str(call["id"]), str(f["arguments"]), ms)
			}
			if f := object(delta["function_call"]); f != nil {
				add(idx, "function", "tool_call", "", str(f["name"]), "", str(f["arguments"]), ms)
			}
		}
	}
	// A tiny SSE splitter used only off the forwarding path, retaining event end
	// offsets for relative timing. CR, LF and CRLF are all accepted.
	var lines [][]byte
	lineStart := 0
	for i := 0; i < len(c.output); i++ {
		if c.output[i] != '\r' && c.output[i] != '\n' {
			continue
		}
		line := c.output[lineStart:i]
		if c.output[i] == '\r' && i+1 < len(c.output) && c.output[i+1] == '\n' {
			i++
		}
		lineStart = i + 1
		if len(line) == 0 {
			if len(lines) > 0 {
				data := bytes.Join(lines, []byte("\n"))
				ms := c.record.LatencyMS
				for _, chunk := range c.chunks {
					if chunk.End >= i+1 {
						ms = chunk.MS
						break
					}
				}
				consume(data, ms)
			}
			lines = nil
		} else if bytes.HasPrefix(bytes.TrimPrefix(line, []byte("\xef\xbb\xbf")), []byte("data:")) {
			_, value, _ := bytes.Cut(line, []byte(":"))
			lines = append(lines, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	// Keep the provider's block order and separate parallel choices.
	choices := map[string][]Block{}
	for _, key := range order {
		choices[roles[key]] = append(choices[roles[key]], *state[key])
	}
	keys := make([]string, 0, len(choices))
	for key := range choices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d.Output = append(d.Output, Message{Role: "assistant", Blocks: choices[key]})
	}
}
func firstNonempty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func providerError(v any) string {
	m := object(v)
	if m == nil {
		return "Provider returned an error."
	}
	typ := str(m["type"])
	message := str(m["message"])
	if typ != "" {
		return typ + ": " + message
	}
	return firstNonempty(message, "Provider returned an error.")
}
