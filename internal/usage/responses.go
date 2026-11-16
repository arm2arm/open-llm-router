package usage

import "bytes"

// A Responses terminal event embeds the complete output and can be much larger
// than maxEvent. Observe its JSON incrementally even when the SSE buffer becomes
// opaque, so a valid long response retains its usage and completion marker.
func (s *Stream) observeResponse(p []byte) {
	for len(p) > 0 {
		c := p[0]
		if c == '\r' || c == '\n' {
			if s.responseData {
				s.response.Write([]byte{'\n'})
			}
			s.responsePrefix, s.responseData, s.responseSkip = 0, false, false
			p = p[1:]
			continue
		}
		if !s.responseData && !s.responseSkip {
			// SSE permits a UTF-8 BOM before the first field. The regular
			// inspector also tolerates it at subsequent line boundaries.
			if s.responsePrefix < 0 {
				index := -s.responsePrefix
				if c != "\xef\xbb\xbf"[index] {
					s.responseSkip = true
				} else if index == 2 {
					s.responsePrefix = 0
				} else {
					s.responsePrefix--
				}
				p = p[1:]
				continue
			}
			if s.responsePrefix == 0 && c == 0xef {
				s.responsePrefix = -1
				p = p[1:]
				continue
			}
			if c != "data:"[s.responsePrefix] {
				s.responseSkip = true
			} else {
				s.responsePrefix++
				if s.responsePrefix == len("data:") {
					s.responseData = true
				}
			}
			p = p[1:]
			continue
		}
		end := bytes.IndexAny(p, "\r\n")
		if end < 0 {
			end = len(p)
		}
		if s.responseData {
			s.response.Write(p[:end])
		}
		p = p[end:]
	}
}

func (s *Stream) finishResponse() {
	value := &s.response
	switch value.Type {
	case "response.completed", "response.incomplete", "response.failed":
		if value.Totals.HasInput {
			s.Totals.InputTokens, s.Totals.HasInput = value.Totals.InputTokens, true
		}
		if value.Totals.HasOutput {
			s.Totals.OutputTokens, s.Totals.HasOutput = value.Totals.OutputTokens, true
		}
		s.Totals.Complete = true
		if value.Type == "response.failed" {
			s.Totals.StreamError = true
		}
	case "error":
		s.Totals.StreamError, s.Totals.Complete = true, true
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if value.HasDelta {
			s.Totals.ContentEvents++
		}
	}
	s.response = JSON{Protocol: Responses, UsageDepth: 2}
}
