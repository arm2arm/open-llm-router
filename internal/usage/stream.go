package usage

import (
	"bytes"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// maxEvent bounds inspection memory. Oversized events still pass through in
// full, but their metadata is not inspected or filtered.
const maxEvent = 1 << 20

// Stream splits SSE on blank lines, preserving every byte of each event,
// including comments, unknown fields and LF/CRLF/CR line endings. Only an
// injected OpenAI usage-only event may be omitted. emit must consume its slice
// before returning; the buffer is reused.
type Stream struct {
	Protocol  string
	HideUsage bool
	Totals    Totals
	emit      func([]byte) error
	buffer    []byte
	data      []byte
	lineLen   int
	cr        bool
	opaque    bool
}

func NewStream(protocol string, hideUsage bool, emit func([]byte) error) *Stream {
	return &Stream{Protocol: protocol, HideUsage: hideUsage, emit: emit}
}

func (s *Stream) append(p []byte) error {
	if !s.opaque && len(s.buffer)+len(p) <= maxEvent {
		s.buffer = append(s.buffer, p...)
		return nil
	}
	if len(s.buffer) > 0 {
		if err := s.emit(s.buffer); err != nil {
			return err
		}
		s.buffer = s.buffer[:0]
	}
	s.opaque = true
	return s.emit(p)
}

func (s *Stream) endLine() error {
	blank := s.lineLen == 0
	s.lineLen = 0
	if !blank {
		return nil
	}
	if !s.opaque && !s.inspect(s.buffer) {
		if err := s.emit(s.buffer); err != nil {
			return err
		}
	}
	s.buffer = s.buffer[:0]
	s.opaque = false
	return nil
}

func (s *Stream) Write(p []byte) error {
	for len(p) > 0 {
		if s.cr {
			s.cr = false
			if p[0] == '\n' {
				if err := s.append(p[:1]); err != nil {
					return err
				}
				p = p[1:]
			}
			if err := s.endLine(); err != nil {
				return err
			}
			continue
		}
		end := bytes.IndexAny(p, "\r\n")
		if end < 0 {
			s.lineLen += len(p)
			return s.append(p)
		}
		s.lineLen += end
		if err := s.append(p[:end+1]); err != nil {
			return err
		}
		c := p[end]
		p = p[end+1:]
		if c == '\r' {
			s.cr = true
		} else if err := s.endLine(); err != nil {
			return err
		}
	}
	return nil
}

// Finish forwards an incomplete tail unchanged. It is not treated as a
// dispatched event. A trailing CR still completes a line, per SSE framing.
func (s *Stream) Finish() error {
	if s.cr {
		s.cr = false
		if err := s.endLine(); err != nil {
			return err
		}
	}
	if len(s.buffer) > 0 {
		if err := s.emit(s.buffer); err != nil {
			return err
		}
		s.buffer = s.buffer[:0]
	}
	return nil
}

func (s *Stream) inspect(event []byte) bool {
	var name, data []byte
	haveData := false
	s.data = s.data[:0]
	for len(event) > 0 {
		end := bytes.IndexAny(event, "\r\n")
		if end < 0 {
			break
		}
		line := bytes.TrimPrefix(event[:end], []byte("\xef\xbb\xbf"))
		eol := 1
		if event[end] == '\r' && len(event) > end+1 && event[end+1] == '\n' {
			eol++
		}
		event = event[end+eol:]
		key, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		if bytes.Equal(key, []byte("event")) {
			name = value
		} else if bytes.Equal(key, []byte("data")) {
			if !haveData {
				data, haveData = value, true
			} else {
				if len(s.data) == 0 {
					s.data = append(s.data, data...)
				}
				s.data = append(s.data, '\n')
				s.data = append(s.data, value...)
				data = s.data
			}
		}
	}
	if !haveData {
		return false
	}
	if s.Protocol == config.ProtocolAnthropic {
		switch string(name) {
		case "message_start":
			s.Totals.readUsage(field(field(data, "message"), "usage"), s.Protocol, true, false)
		case "message_delta":
			s.Totals.readUsage(field(data, "usage"), s.Protocol, false, true)
		case "content_block_delta":
			switch string(field(field(data, "delta"), "type")) {
			case `"text_delta"`, `"thinking_delta"`, `"input_json_delta"`:
				s.Totals.ContentEvents++
			}
		case "message_stop":
			s.Totals.Complete = true
		case "error":
			s.Totals.StreamError, s.Totals.Complete = true, true
		}
		return false
	}
	if bytes.Equal(trim(data), []byte("[DONE]")) {
		s.Totals.Complete = true
		return false
	}
	if raw := field(data, "error"); len(raw) > 0 && !bytes.Equal(trim(raw), []byte("null")) {
		s.Totals.StreamError = true
	}
	if raw := field(data, "usage"); len(raw) > 0 && raw[0] == '{' {
		valid := s.Totals.readUsage(raw, s.Protocol, true, true)
		choices := trim(field(data, "choices"))
		if valid && s.HideUsage && len(choices) >= 2 && choices[0] == '[' && choices[len(choices)-1] == ']' && len(trim(choices[1:len(choices)-1])) == 0 {
			return true
		}
	}
	if arrayAny(field(data, "choices"), openAIContent) {
		s.Totals.ContentEvents++
	}
	return false
}

func openAIContent(choice []byte) bool {
	if nonemptyString(field(choice, "text")) {
		return true
	}
	delta := field(choice, "delta")
	for _, key := range []string{"content", "reasoning_content", "refusal"} {
		if nonemptyString(field(delta, key)) {
			return true
		}
	}
	return arrayAny(field(delta, "tool_calls"), func(call []byte) bool {
		return nonemptyString(field(field(call, "function"), "arguments"))
	})
}
