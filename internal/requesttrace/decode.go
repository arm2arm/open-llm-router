package requesttrace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DetailVersion 3 omits unsafe fragment text from stream events and frames.
const DetailVersion = 3

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
	// Hash identifies identical messages across the requests of an agent loop.
	Hash string `json:"hash,omitempty"`
}
type Event struct {
	MS   int64  `json:"ms"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Truncated   bool            `json:"truncated,omitempty"`
}
type Frame struct {
	MS   int64           `json:"ms"`
	Data json.RawMessage `json:"data"`
}
type Change struct {
	Field  string `json:"field"`
	Action string `json:"action"`
	From   any    `json:"from,omitempty"`
	To     any    `json:"to,omitempty"`
}
type Attempt struct {
	Replica string `json:"replica"`
	QueueMS int64  `json:"queue_ms"`
	MS      int64  `json:"ms"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
}
type Routing struct {
	RequestedModel string    `json:"requested_model,omitempty"`
	Model          string    `json:"model,omitempty"`
	UpstreamModel  string    `json:"upstream_model,omitempty"`
	Changes        []Change  `json:"changes"`
	Attempts       []Attempt `json:"attempts"`
	AdmissionMS    int64     `json:"admission_ms"`
	Headers        []string  `json:"headers"`
	UpstreamStatus int       `json:"upstream_status,omitempty"`
	ContentType    string    `json:"content_type,omitempty"`
}
type Result struct {
	ID                string             `json:"id,omitempty"`
	Model             string             `json:"model,omitempty"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
	FinishReasons     []string           `json:"finish_reasons"`
	Usage             map[string]float64 `json:"usage"`
}
type Summary struct {
	InputMessages   int    `json:"input_messages"`
	OutputMessages  int    `json:"output_messages"`
	ToolCalls       int    `json:"tool_calls"`
	ToolResults     int    `json:"tool_results"`
	Tools           int    `json:"tools"`
	FinishReason    string `json:"finish_reason,omitempty"`
	CachedTokens    int64  `json:"cached_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	Truncated       bool   `json:"truncated"`
}
type Detail struct {
	Version    int             `json:"v"`
	Operation  string          `json:"operation"`
	Input      []Message       `json:"input"`
	Output     []Message       `json:"output"`
	Events     []Event         `json:"events"`
	Parameters map[string]any  `json:"parameters"`
	Tools      []Tool          `json:"tools"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
	Frames     []Frame         `json:"frames"`
	Routing    Routing         `json:"routing"`
	Result     Result          `json:"result"`
	Summary    Summary         `json:"summary"`
	Truncated  bool            `json:"truncated"`
	Warnings   []string        `json:"warnings"`
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
	return blocksDepth(v, 0)
}

func blocksDepth(v any, depth int) []Block {
	out := rawBlocksDepth(v, depth)
	for i := range out {
		out[i].Text = payloadText(out[i].Text)
	}
	return out
}

// payloadText replaces text that is entirely a data URI or binary encoding.
func payloadText(s string) string {
	if strings.HasPrefix(s, "data:") && len(s) > 64 || looksBinary(s) {
		return cleanString(s, len(s))
	}
	return s
}

func rawBlocksDepth(v any, depth int) []Block {
	if depth > rawMaxDepth {
		return []Block{{Type: "text", Text: marker("nested content omitted")}}
	}
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
		case "refusal":
			out = append(out, Block{Type: "refusal", Text: str(m["refusal"])})
		case "redacted_thinking":
			out = append(out, Block{Type: "reasoning", Text: "[Provider redacted reasoning]"})
		case "tool_use":
			out = append(out, Block{Type: "tool_call", ID: str(m["id"]), Name: str(m["name"]), Arguments: compact(m["input"])})
		case "tool_result":
			out = append(out, Block{Type: "tool_result", ID: str(m["tool_use_id"]), Text: contentTextDepth(m["content"], depth+1)})
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
	return contentTextDepth(v, 0)
}

func contentTextDepth(v any, depth int) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	for _, b := range rawBlocksDepth(v, depth) {
		if b.Text != "" {
			parts = append(parts, payloadText(b.Text))
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

// Content fields normalized into Input; everything else is a parameter.
var contentKeys = map[string]bool{"messages": true, "system": true, "instructions": true, "prompt": true, "input": true, "tools": true}

func (c *Capture) decode() Detail {
	d := Detail{Version: DetailVersion, Operation: c.operation, Input: []Message{}, Output: []Message{}, Events: []Event{}, Parameters: map[string]any{}, Tools: []Tool{}, Frames: []Frame{}, Routing: c.routing, Result: Result{FinishReasons: []string{}, Usage: map[string]float64{}}, Warnings: []string{}, Truncated: c.truncated}
	if d.Routing.Changes == nil {
		d.Routing.Changes = []Change{}
	}
	if d.Routing.Attempts == nil {
		d.Routing.Attempts = []Attempt{}
	}
	if d.Routing.Headers == nil {
		d.Routing.Headers = []string{}
	}
	var input map[string]any
	if len(c.input) > 0 && decodeJSON(c.input, &input) == nil {
		if sys, ok := input["system"]; ok {
			d.Input = append(d.Input, Message{Role: "system", Blocks: blocks(sys)})
		}
		if instructions, ok := input["instructions"]; ok {
			d.Input = append(d.Input, Message{Role: "system", Blocks: blocks(instructions)})
		}
		for _, v := range list(input["messages"]) {
			d.Input = append(d.Input, message(object(v)))
		}
		for _, key := range []string{"prompt", "input"} {
			if v, ok := input[key]; ok {
				if c.operation == "responses" && key == "input" {
					d.Input = append(d.Input, responseMessages(v)...)
					continue
				}
				text := str(v)
				if text == "" {
					text = "[Non-text input omitted]"
				}
				d.Input = append(d.Input, Message{Role: "user", Blocks: []Block{{Type: "text", Text: text}}})
			}
		}
		// Every non-content field is a parameter, sanitized like the raw view.
		for key, v := range input {
			if contentKeys[key] {
				continue
			}
			if why, ok := omittedKeys[key]; ok {
				d.Parameters[key] = marker(why)
				continue
			}
			d.Parameters[key] = clean(v, rawStringCap, 0)
		}
		for _, v := range list(input["tools"]) {
			d.Tools = append(d.Tools, tool(object(v)))
		}
		if len(d.Tools) > 0 {
			names := make([]string, 0, len(d.Tools))
			for _, t := range d.Tools {
				names = append(names, t.Name)
			}
			d.Parameters["tools"] = names
		}
		if d.Request = cleanRaw(input, MaxRaw); d.Request == nil {
			d.Warnings = append(d.Warnings, "Raw request preview omitted: above the 128 KiB preview limit after removing payloads.")
		}
	} else if len(c.input) == 0 {
		d.Warnings = append(d.Warnings, "Input unavailable or above the 1 MiB capture limit.")
	}
	if c.limitInput(d.Input) {
		d.Truncated = true
		d.Warnings = append(d.Warnings, "Input text above 384 KiB was shortened in this view.")
	}
	if c.record.IsStream {
		c.decodeStream(&d)
	} else if c.jsonResponse {
		var m map[string]any
		if decodeJSON(c.output, &m) == nil {
			if c.operation == "responses" {
				d.Output = append(d.Output, responseMessages(m["output"])...)
				d.Result.finish(responseFinish(m))
			}
			for _, v := range list(m["choices"]) {
				choice := object(v)
				if msg := object(choice["message"]); msg != nil {
					d.Output = append(d.Output, message(msg))
				} else if s := str(choice["text"]); s != "" {
					d.Output = append(d.Output, Message{Role: "assistant", Blocks: []Block{{Type: "text", Text: s}}})
				}
				d.Result.finish(str(choice["finish_reason"]))
			}
			if _, ok := m["content"]; ok {
				d.Output = append(d.Output, message(m))
			}
			d.Result.finish(str(m["stop_reason"]))
			d.Result.meta(m)
			if m["error"] != nil {
				d.Events = append(d.Events, Event{MS: c.record.LatencyMS, Type: "error", Text: providerError(m["error"])})
			}
			if _, ok := m["data"]; ok && c.operation == "embeddings" {
				d.Warnings = append(d.Warnings, "Embedding vectors are omitted.")
			}
			d.Response = cleanRaw(m, MaxRaw)
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
	for _, msgs := range [][]Message{d.Input, d.Output} {
		for i := range msgs {
			msgs[i].Hash = hashMessage(msgs[i])
		}
	}
	var parametersLimited bool
	d.Parameters, parametersLimited = boundParameters(d.Parameters)
	if parametersLimited {
		d.Truncated = true
		d.Warnings = append(d.Warnings, "Some parameters omitted: above the 32 KiB parameters limit.")
	}
	d.Summary = summarize(&d)
	return d
}

func tool(m map[string]any) Tool {
	def := m
	if f := object(m["function"]); f != nil {
		def = f
	}
	t := Tool{Name: str(def["name"])}
	t.Description, _ = cut(str(def["description"]), 2<<10)
	schema := def["parameters"]
	if schema == nil {
		schema = def["input_schema"]
	}
	if schema == nil && str(m["type"]) != "" && str(m["type"]) != "function" && str(m["type"]) != "custom" {
		// Provider built-in tools (web_search, computer, …) have no schema.
		t.Name = firstNonempty(t.Name, str(m["type"]))
	}
	if schema != nil {
		if t.Schema = cleanRaw(schema, 8<<10); t.Schema == nil {
			t.Truncated = true
		}
	}
	return t
}

// limitInput shortens normalized input text past the stored budget.
func (c *Capture) limitInput(msgs []Message) bool {
	budget := MaxInputText
	short := false
	for i := range msgs {
		for j := range msgs[i].Blocks {
			b := &msgs[i].Blocks[j]
			for _, field := range []*string{&b.Text, &b.Arguments} {
				if len(*field) <= budget {
					budget -= len(*field)
					continue
				}
				kept, _ := cut(*field, max(budget, 0))
				*field = kept + "…" + marker(fmt.Sprintf("+%d bytes not stored", len(*field)-len(kept)))
				budget = 0
				short = true
			}
		}
	}
	return short
}

func (r *Result) finish(reason string) {
	if reason != "" && !contains(r.FinishReasons, reason) {
		r.FinishReasons = append(r.FinishReasons, reason)
	}
}
func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// meta takes identifiers and numeric usage, never free-form provider fields.
func (r *Result) meta(m map[string]any) {
	if id, _ := cut(str(m["id"]), 128); id != "" {
		r.ID = id
	}
	if model, _ := cut(str(m["model"]), 128); model != "" {
		r.Model = model
	}
	if fp, _ := cut(str(m["system_fingerprint"]), 128); fp != "" {
		r.SystemFingerprint = fp
	}
	if u := object(m["usage"]); u != nil {
		numbers(u, "", r.Usage)
	}
}

func hashMessage(m Message) string {
	h := sha256.New()
	h.Write([]byte(m.Role))
	for _, b := range m.Blocks {
		fmt.Fprintf(h, "\x00%s\x00%s\x00%s\x00%s\x00%s", b.Type, b.Name, b.ID, b.Arguments, b.Text)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func summarize(d *Detail) Summary {
	s := Summary{InputMessages: len(d.Input), OutputMessages: len(d.Output), Tools: len(d.Tools), Truncated: d.Truncated}
	for _, m := range d.Output {
		for _, b := range m.Blocks {
			if b.Type == "tool_call" {
				s.ToolCalls++
			}
		}
	}
	for _, m := range d.Input {
		for _, b := range m.Blocks {
			if b.Type == "tool_result" {
				s.ToolResults++
			}
		}
	}
	if len(d.Result.FinishReasons) > 0 {
		s.FinishReason = d.Result.FinishReasons[0]
	}
	u := d.Result.Usage
	s.CachedTokens = int64(u["prompt_tokens_details.cached_tokens"] + u["cache_read_input_tokens"] + u["input_tokens_details.cached_tokens"])
	s.ReasoningTokens = int64(u["completion_tokens_details.reasoning_tokens"] + u["output_tokens_details.reasoning_tokens"])
	return s
}

// conversationKey groups an agent loop's requests: the client's session ID
// when provided, otherwise its system prompt and first user message. It is
// keyed by the user, so equal prompts of different users never correlate.
func (c *Capture) conversationKey(d Detail) string {
	h := sha256.New()
	h.Write([]byte(c.record.UserID))
	h.Write([]byte{0})
	if c.session != "" {
		h.Write([]byte("session\x00" + c.session))
		return "s" + hex.EncodeToString(h.Sum(nil))[:20]
	}
	var system, first string
	for _, m := range d.Input {
		if m.Role == "system" || m.Role == "developer" {
			if system == "" {
				system = m.Hash
			}
		} else if m.Role == "user" {
			first = m.Hash
			break
		}
	}
	if first == "" {
		return ""
	}
	h.Write([]byte(c.operation + "\x00" + system + "\x00" + first))
	return "c" + hex.EncodeToString(h.Sum(nil))[:20]
}

func (c *Capture) decodeStream(d *Detail) {
	state := map[string]*Block{}
	responseFinal := map[string][]Block{}
	var responseOutput []Message
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
			// Token fragments cannot be redacted independently: a credential or
			// operator pattern may span any number of events. The conversation
			// reconstructs and redacts the complete field; the timeline keeps
			// event kinds and timing without duplicating unsafe fragments.
			d.Events = append(d.Events, Event{MS: ms, Type: kind})
		} else {
			d.Truncated = true
		}
	}
	frameBytes := 0
	frame := func(raw json.RawMessage, ms int64) {
		if raw == nil {
			return
		}
		if len(d.Frames) >= MaxFrames || frameBytes+len(raw) > MaxFrameRaw {
			if !contains(d.Warnings, framesWarning) {
				d.Warnings = append(d.Warnings, framesWarning)
			}
			return
		}
		frameBytes += len(raw)
		d.Frames = append(d.Frames, Frame{MS: ms, Data: raw})
	}
	consume := func(data []byte, ms int64) {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			frame(json.RawMessage(`"[DONE]"`), ms)
			if len(d.Events) < MaxEvents {
				d.Events = append(d.Events, Event{MS: ms, Type: "complete"})
			}
			return
		}
		var m map[string]any
		if decodeJSON(data, &m) != nil {
			return
		}
		frame(cleanRaw(streamPreview(m), 4<<10), ms)
		d.Result.meta(m)
		kind := str(m["type"])
		index := fmt.Sprint(m["index"])
		switch kind {
		case "response.output_item.added":
			item := object(m["item"])
			if str(item["type"]) == "function_call" || str(item["type"]) == "custom_tool_call" {
				add(responseIndex(m), "call", "tool_call", "", str(item["name"]), str(item["call_id"]), "", ms)
			}
		case "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			typ, part := "text", fmt.Sprint(m["content_index"])
			if kind == "response.refusal.delta" {
				typ = "refusal"
			} else if strings.Contains(kind, "reasoning") {
				typ, part = "reasoning", fmt.Sprint(m["summary_index"])
			}
			add(responseIndex(m), typ+part, typ, str(m["delta"]), "", "", "", ms)
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			add(responseIndex(m), "call", "tool_call", "", "", "", str(m["delta"]), ms)
		case "response.output_item.done":
			var complete []Block
			for _, msg := range responseMessages([]any{m["item"]}) {
				complete = append(complete, msg.Blocks...)
			}
			responseFinal[responseIndex(m)] = complete
		case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
			response := object(m["response"])
			d.Result.meta(response)
			if kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
				d.Result.finish(responseFinish(response))
				if output := responseMessages(response["output"]); len(output) > 0 {
					responseOutput = output
				}
				if len(d.Events) < MaxEvents {
					event := Event{MS: ms, Type: "complete"}
					if kind == "response.failed" {
						event.Type, event.Text = "error", providerError(response["error"])
					}
					d.Events = append(d.Events, event)
				}
			}
		case "message_start":
			d.Result.meta(object(m["message"]))
		case "message_delta":
			d.Result.finish(str(object(m["delta"])["stop_reason"]))
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
			d.Result.finish(str(choice["finish_reason"]))
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
	for key, blocks := range responseFinal {
		choices[key] = blocks
	}
	keys := make([]string, 0, len(choices))
	for key := range choices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d.Output = append(d.Output, Message{Role: "assistant", Blocks: choices[key]})
	}
	if len(responseOutput) > 0 {
		d.Output = responseOutput
	}
}

const framesWarning = "Raw stream frames are limited to the first 200 (64 KiB); the timeline and conversation cover the full capture."

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
