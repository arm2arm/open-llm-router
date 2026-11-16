package mockllm

import (
	"encoding/json"
	"net/http"
	"time"
)

type responsesRequest struct {
	Model           string `json:"model"`
	Input           any    `json:"input"`
	Instructions    string `json:"instructions"`
	Stream          bool   `json:"stream"`
	MaxOutputTokens *int   `json:"max_output_tokens"`
	Tools           []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Text       struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"text"`
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request, c *call) {
	var req responsesRequest
	if !c.decode(w, &req, &req.Model) {
		return
	}
	prompt := countTokens(req.Input) + countTokens(req.Instructions)
	n, truncated := c.outputTokens(req.MaxOutputTokens)
	status, terminal := "completed", "response.completed"
	var incomplete any
	if truncated {
		status, terminal = "incomplete", "response.incomplete"
		incomplete = obj{"reason": "max_output_tokens"}
	}
	tool := ""
	if len(req.Tools) > 0 && req.Tools[0].Type == "function" && string(req.ToolChoice) != `"none"` {
		tool = req.Tools[0].Name
		var named struct{ Name string }
		if json.Unmarshal(req.ToolChoice, &named) == nil && named.Name != "" {
			tool = named.Name
		}
		if input, ok := req.Input.([]any); ok && len(input) > 0 {
			if last, ok := input[len(input)-1].(map[string]any); ok && last["type"] == "function_call_output" {
				tool = ""
			}
		}
	}
	text := Text(n)
	if req.Text.Format.Type == "json_schema" || req.Text.Format.Type == "json_object" {
		b, _ := json.Marshal(obj{"answer": text})
		text = string(b)
	}
	id, itemID, callID := s.nextID("resp_"), s.nextID("msg_"), ""
	content := obj{"type": "output_text", "text": text, "annotations": []obj{}, "logprobs": []obj{}}
	item := obj{"id": itemID, "type": "message", "status": "completed", "role": "assistant", "content": []obj{content}}
	if tool != "" {
		n, status, terminal, incomplete = ToolCallTokens, "completed", "response.completed", nil
		callID = s.nextID("call_")
		item = obj{"id": itemID, "type": "function_call", "status": "completed", "call_id": callID, "name": tool, "arguments": "{}"}
	}
	answer := obj{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "model": req.Model,
		"status": status, "error": nil, "incomplete_details": incomplete, "output": []obj{item},
		"store": false, "background": false, "parallel_tool_calls": true, "tools": req.Tools, "tool_choice": "auto",
		"usage": obj{"input_tokens": prompt, "output_tokens": n, "total_tokens": prompt + n,
			"input_tokens_details": obj{"cached_tokens": 0}, "output_tokens_details": obj{"reasoning_tokens": 0}},
	}
	if !req.Stream {
		c.respond(w, r, n, answer)
		return
	}
	st := s.startStream(w, r, c, n)
	sequence := 0
	emit := func(kind string, value obj, token bool) {
		value["type"], value["sequence_number"] = kind, sequence
		sequence++
		if token {
			st.token(kind, value)
		} else {
			st.event(kind, value)
		}
	}
	initial := obj{"id": id, "object": "response", "created_at": answer["created_at"], "model": req.Model, "status": "in_progress", "output": []obj{}, "usage": nil, "error": nil}
	emit("response.created", obj{"response": initial}, false)
	emit("response.in_progress", obj{"response": initial}, false)
	if tool != "" {
		emit("response.output_item.added", obj{"output_index": 0, "item": obj{"id": itemID, "type": "function_call", "status": "in_progress", "call_id": callID, "name": tool, "arguments": ""}}, false)
		for _, piece := range []string{"{", "}"} {
			emit("response.function_call_arguments.delta", obj{"item_id": itemID, "output_index": 0, "delta": piece}, true)
		}
		emit("response.function_call_arguments.done", obj{"item_id": itemID, "output_index": 0, "arguments": "{}"}, false)
	} else {
		emit("response.output_item.added", obj{"output_index": 0, "item": obj{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []obj{}}}, false)
		emit("response.content_part.added", obj{"item_id": itemID, "output_index": 0, "content_index": 0, "part": obj{"type": "output_text", "text": "", "annotations": []obj{}}}, false)
		pieces := tokens(n)
		if req.Text.Format.Type == "json_schema" || req.Text.Format.Type == "json_object" {
			pieces = []string{text}
		}
		for _, piece := range pieces {
			emit("response.output_text.delta", obj{"item_id": itemID, "output_index": 0, "content_index": 0, "delta": piece, "logprobs": []obj{}}, true)
		}
		emit("response.output_text.done", obj{"item_id": itemID, "output_index": 0, "content_index": 0, "text": text, "logprobs": []obj{}}, false)
		emit("response.content_part.done", obj{"item_id": itemID, "output_index": 0, "content_index": 0, "part": content}, false)
	}
	emit("response.output_item.done", obj{"output_index": 0, "item": item}, false)
	emit(terminal, obj{"response": answer}, false)
}
