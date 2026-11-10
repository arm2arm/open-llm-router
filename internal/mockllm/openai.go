package mockllm

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"time"
)

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

func openAIUsage(prompt, completion int) obj {
	return obj{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
}

// finishReason is the finish_reason of a text answer.
func finishReason(truncated bool) string {
	if truncated {
		return "length"
	}
	return "stop"
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request, c *call) {
	data := make([]obj, len(c.Models))
	for i, model := range c.Models {
		data[i] = obj{"id": model, "object": "model", "created": time.Now().Unix(), "owned_by": "mock-llm"}
	}
	writeJSON(w, http.StatusOK, obj{"object": "list", "data": data})
}

type chatRequest struct {
	Model               string              `json:"model"`
	Messages            []message           `json:"messages"`
	Stream              bool                `json:"stream"`
	StreamOptions       openAIStreamOptions `json:"stream_options"`
	MaxTokens           *int                `json:"max_tokens"`
	MaxCompletionTokens *int                `json:"max_completion_tokens"`
	Tools               []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// toolToCall returns the tool that the answer calls, if it calls one.
func (r *chatRequest) toolToCall() (name string, ok bool) {
	if len(r.Tools) == 0 {
		return "", false
	}
	if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == "tool" {
		return "", false
	}
	var mode string
	if json.Unmarshal(r.ToolChoice, &mode) == nil && mode == "none" {
		return "", false
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(r.ToolChoice, &named) == nil && named.Function.Name != "" {
		return named.Function.Name, true
	}
	return r.Tools[0].Function.Name, true
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request, c *call) {
	var req chatRequest
	if !c.decode(w, &req, &req.Model) {
		return
	}
	id, created := s.nextID("chatcmpl-"), time.Now().Unix()
	prompt := countMessages(req.Messages)
	n, truncated := c.outputTokens(req.MaxCompletionTokens, req.MaxTokens)
	finish := finishReason(truncated)
	tool, callsTool := req.toolToCall()
	callID := ""
	if callsTool {
		n, finish, callID = ToolCallTokens, "tool_calls", s.nextID("call_")
	}

	if !req.Stream {
		msg := obj{"role": "assistant", "content": Text(n)}
		if callsTool {
			msg = obj{"role": "assistant", "content": nil, "tool_calls": []obj{
				{"id": callID, "type": "function", "function": obj{"name": tool, "arguments": "{}"}},
			}}
		}
		c.respond(w, r, n, obj{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []obj{{"index": 0, "message": msg, "finish_reason": finish, "logprobs": nil}},
			"usage":   openAIUsage(prompt, n),
		})
		return
	}

	chunk := func(delta obj, finish any) obj {
		return obj{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []obj{{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}},
		}
	}
	st := s.startStream(w, r, c, n)
	if callsTool {
		st.event("", chunk(obj{"role": "assistant", "content": nil, "tool_calls": []obj{
			{"index": 0, "id": callID, "type": "function", "function": obj{"name": tool, "arguments": ""}},
		}}, nil))
		for _, piece := range []string{"{", "}"} {
			st.token("", chunk(obj{"tool_calls": []obj{{"index": 0, "function": obj{"arguments": piece}}}}, nil))
		}
	} else {
		st.event("", chunk(obj{"role": "assistant", "content": ""}, nil))
		for _, tok := range tokens(n) {
			st.token("", chunk(obj{"content": tok}, nil))
		}
	}
	st.event("", chunk(obj{}, finish))
	if req.StreamOptions.IncludeUsage {
		st.event("", obj{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []obj{}, "usage": openAIUsage(prompt, n),
		})
	}
	st.event("", "[DONE]")
}

func (s *Server) completions(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Model         string              `json:"model"`
		Prompt        any                 `json:"prompt"`
		Stream        bool                `json:"stream"`
		StreamOptions openAIStreamOptions `json:"stream_options"`
		MaxTokens     *int                `json:"max_tokens"`
	}
	if !c.decode(w, &req, &req.Model) {
		return
	}
	id, created := s.nextID("cmpl-"), time.Now().Unix()
	prompt := countTokens(req.Prompt)
	n, truncated := c.outputTokens(req.MaxTokens)
	completion := func(text string, finish any) obj {
		return obj{
			"id": id, "object": "text_completion", "created": created, "model": req.Model,
			"choices": []obj{{"index": 0, "text": text, "finish_reason": finish, "logprobs": nil}},
		}
	}

	if !req.Stream {
		answer := completion(Text(n), finishReason(truncated))
		answer["usage"] = openAIUsage(prompt, n)
		c.respond(w, r, n, answer)
		return
	}
	st := s.startStream(w, r, c, n)
	for _, tok := range tokens(n) {
		st.token("", completion(tok, nil))
	}
	st.event("", completion("", finishReason(truncated)))
	if req.StreamOptions.IncludeUsage {
		st.event("", obj{
			"id": id, "object": "text_completion", "created": created, "model": req.Model,
			"choices": []obj{}, "usage": openAIUsage(prompt, n),
		})
	}
	st.event("", "[DONE]")
}

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request, c *call) {
	var req struct {
		Model          string `json:"model"`
		Input          any    `json:"input"`
		EncodingFormat string `json:"encoding_format"`
		Dimensions     int    `json:"dimensions"`
	}
	if !c.decode(w, &req, &req.Model) {
		return
	}
	// One embedding per input: a list of strings is several inputs, anything
	// else (a string, a list of token IDs) is one.
	inputs := []any{req.Input}
	if list, ok := req.Input.([]any); ok && len(list) > 0 {
		if _, strings := list[0].(string); strings {
			inputs = list
		}
	}
	if req.Dimensions <= 0 {
		req.Dimensions = 8
	}
	data := make([]obj, len(inputs))
	for i, input := range inputs {
		vector := embedding(input, req.Dimensions)
		data[i] = obj{"object": "embedding", "index": i, "embedding": vector}
		if req.EncodingFormat == "base64" {
			raw := make([]byte, 0, 4*len(vector))
			for _, x := range vector {
				raw = binary.LittleEndian.AppendUint32(raw, math.Float32bits(x))
			}
			data[i]["embedding"] = base64.StdEncoding.EncodeToString(raw)
		}
	}
	prompt := countTokens(req.Input)
	c.respond(w, r, 0, obj{
		"object": "list", "data": data, "model": req.Model,
		"usage": obj{"prompt_tokens": prompt, "total_tokens": prompt},
	})
}

// embedding returns a vector that depends only on the input: values in
// [-1, 1) drawn from a generator seeded with a hash of the input.
func embedding(input any, dimensions int) []float32 {
	text, _ := json.Marshal(input)
	h := fnv.New64a()
	h.Write(text)
	state := h.Sum64()
	vector := make([]float32, dimensions)
	for i := range vector {
		state = state*6364136223846793005 + 1442695040888963407
		vector[i] = float32(state>>40)/float32(1<<23) - 1
	}
	return vector
}
