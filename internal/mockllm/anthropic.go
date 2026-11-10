package mockllm

import (
	"net/http"
	"strings"
)

type messagesRequest struct {
	Model     string    `json:"model"`
	System    any       `json:"system"`
	Messages  []message `json:"messages"`
	MaxTokens *int      `json:"max_tokens"`
	Stream    bool      `json:"stream"`
	Tools     []struct {
		Name string `json:"name"`
	} `json:"tools"`
	ToolChoice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
	Thinking struct {
		Type string `json:"type"`
	} `json:"thinking"`
}

func (r *messagesRequest) inputTokens() int {
	return countTokens(r.System) + countMessages(r.Messages)
}

// toolToCall returns the tool that the answer calls, if it calls one.
func (r *messagesRequest) toolToCall() (name string, ok bool) {
	if len(r.Tools) == 0 || r.ToolChoice.Type == "none" {
		return "", false
	}
	if n := len(r.Messages); n > 0 {
		blocks, _ := r.Messages[n-1].Content.([]any)
		for _, block := range blocks {
			if b, _ := block.(map[string]any); b["type"] == "tool_result" {
				return "", false
			}
		}
	}
	if r.ToolChoice.Name != "" {
		return r.ToolChoice.Name, true
	}
	return r.Tools[0].Name, true
}

func (s *Server) messagesCountTokens(w http.ResponseWriter, _ *http.Request, c *call) {
	var req messagesRequest
	if !c.decode(w, &req, &req.Model) {
		return
	}
	writeJSON(w, http.StatusOK, obj{"input_tokens": req.inputTokens()})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, c *call) {
	var req messagesRequest
	if !c.decode(w, &req, &req.Model) {
		return
	}
	id := s.nextID("msg_")
	input := req.inputTokens()
	n, truncated := c.outputTokens(req.MaxTokens)
	stop := "end_turn"
	if truncated {
		stop = "max_tokens"
	}
	tool, callsTool := req.toolToCall()
	toolID := ""
	if callsTool {
		n, stop, toolID = ToolCallTokens, "tool_use", s.nextID("toolu_")
	}
	thinks := c.Thinking || (req.Thinking.Type != "" && req.Thinking.Type != "disabled")
	output := n
	if thinks {
		output += ThinkingTokens
	}
	header := func(content []obj, stopReason any, outputTokens int) obj {
		return obj{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content": content, "stop_reason": stopReason, "stop_sequence": nil,
			"usage": obj{"input_tokens": input, "output_tokens": outputTokens},
		}
	}

	if !req.Stream {
		var content []obj
		if thinks {
			content = append(content, obj{"type": "thinking", "thinking": ThinkingText, "signature": "mock-signature"})
		}
		if callsTool {
			content = append(content, obj{"type": "tool_use", "id": toolID, "name": tool, "input": obj{}})
		} else {
			content = append(content, obj{"type": "text", "text": Text(n)})
		}
		c.respond(w, r, output, header(content, stop, output))
		return
	}

	// The event sequence of the Anthropic API: message_start, then for each
	// content block start, deltas and stop, then message_delta with the stop
	// reason and the usage, then message_stop.
	st := s.startStream(w, r, c, output)
	st.event("message_start", obj{"type": "message_start", "message": header([]obj{}, nil, 0)})
	index := 0
	block := func(start obj, deltas []obj, last ...obj) {
		st.event("content_block_start", obj{"type": "content_block_start", "index": index, "content_block": start})
		if index == 0 {
			st.event("ping", obj{"type": "ping"})
		}
		for _, delta := range deltas {
			st.token("content_block_delta", obj{"type": "content_block_delta", "index": index, "delta": delta})
		}
		for _, delta := range last {
			st.event("content_block_delta", obj{"type": "content_block_delta", "index": index, "delta": delta})
		}
		st.event("content_block_stop", obj{"type": "content_block_stop", "index": index})
		index++
	}
	if thinks {
		var deltas []obj
		for i, word := range strings.Fields(ThinkingText) {
			if i > 0 {
				word = " " + word
			}
			deltas = append(deltas, obj{"type": "thinking_delta", "thinking": word})
		}
		block(obj{"type": "thinking", "thinking": ""}, deltas, obj{"type": "signature_delta", "signature": "mock-signature"})
	}
	if callsTool {
		block(obj{"type": "tool_use", "id": toolID, "name": tool, "input": obj{}}, []obj{
			{"type": "input_json_delta", "partial_json": "{"},
			{"type": "input_json_delta", "partial_json": "}"},
		})
	} else {
		var deltas []obj
		for _, tok := range tokens(n) {
			deltas = append(deltas, obj{"type": "text_delta", "text": tok})
		}
		block(obj{"type": "text", "text": ""}, deltas)
	}
	st.event("message_delta", obj{
		"type":  "message_delta",
		"delta": obj{"stop_reason": stop, "stop_sequence": nil},
		"usage": obj{"input_tokens": input, "output_tokens": output},
	})
	st.event("message_stop", obj{"type": "message_stop"})
}
