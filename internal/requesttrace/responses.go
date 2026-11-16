package requesttrace

import "fmt"

// responseMessages normalizes only content fields already allowed by the
// trace privacy policy. Encrypted reasoning, signatures and binary attachments
// are deliberately absent, including on a completed response's output items.
func responseMessages(v any) []Message {
	if text, ok := v.(string); ok {
		return []Message{{Role: "user", Blocks: blocks(text)}}
	}
	out := []Message{}
	for _, v := range list(v) {
		item := object(v)
		var b []Block
		switch str(item["type"]) {
		case "", "message":
			out = append(out, message(item))
			continue
		case "function_call", "custom_tool_call":
			args := str(item["arguments"])
			if str(item["type"]) == "custom_tool_call" {
				args = str(item["input"])
			}
			b = []Block{{Type: "tool_call", ID: str(item["call_id"]), Name: str(item["name"]), Arguments: payloadText(args)}}
		case "function_call_output", "custom_tool_call_output":
			out = append(out, Message{Role: "tool", Blocks: []Block{{Type: "tool_result", ID: str(item["call_id"]), Text: payloadText(contentText(item["output"]))}}})
			continue
		case "reasoning":
			for _, key := range []string{"summary", "content"} {
				for _, part := range list(item[key]) {
					part := object(part)
					if str(part["type"]) == "summary_text" || str(part["type"]) == "reasoning_text" {
						b = append(b, Block{Type: "reasoning", Text: payloadText(str(part["text"]))})
					}
				}
			}
		case "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "mcp_call", "image_generation_call":
			b = []Block{{Type: "tool_call", ID: str(item["id"]), Name: firstNonempty(str(item["name"]), str(item["type"]))}}
		}
		if len(b) > 0 {
			out = append(out, Message{Role: "assistant", Blocks: b})
		}
	}
	return out
}

func responseFinish(m map[string]any) string {
	return firstNonempty(str(object(m["incomplete_details"])["reason"]), str(m["status"]))
}

func responseIndex(m map[string]any) string {
	// Stable item order when there are more than ten output items.
	i, _ := m["output_index"].(float64)
	return fmt.Sprintf("%020.0f", i)
}
