package usage

import (
	"bytes"
	"encoding/json"

	"github.com/arm2arm/open-llm-router/internal/config"
)

const maxUsage = 64 << 10

// JSON observes the top-level usage member incrementally. Other values,
// including arbitrarily large content strings and embeddings, are skipped
// without buffering them. Only the small numeric usage object is decoded.
type JSON struct {
	Protocol string
	Totals   Totals
	depth    int
	quoted   bool
	escaped  bool
	key      bool
	keyBytes []byte
	wantKey  bool
	usageKey bool
	wait     bool
	capture  bool
	value    []byte
}

func (j *JSON) Write(p []byte) {
	for _, c := range p {
		if j.capture && len(j.value) <= maxUsage {
			j.value = append(j.value, c)
		}
		if j.quoted {
			if j.key && len(j.keyBytes) < 64 {
				j.keyBytes = append(j.keyBytes, c)
			}
			if j.escaped {
				j.escaped = false
			} else if c == '\\' {
				j.escaped = true
			} else if c == '"' {
				j.quoted = false
				if j.key {
					j.usageKey = keyEquals(j.keyBytes, "usage")
					j.key = false
				}
			}
			continue
		}
		if j.wait && !space(c) {
			j.wait = false
			if c == '{' {
				j.capture = true
				j.value = append(j.value[:0], c)
			}
		}
		switch c {
		case '"':
			j.quoted = true
			if j.depth == 1 && j.wantKey {
				j.key, j.wantKey = true, false
				j.keyBytes = append(j.keyBytes[:0], c)
			}
		case '{', '[':
			j.depth++
			if j.depth == 1 && c == '{' {
				j.wantKey = true
			}
		case '}', ']':
			j.depth--
			if j.capture && j.depth == 1 {
				if len(j.value) <= maxUsage {
					j.Totals.readUsage(j.value, j.Protocol, true, true)
				}
				j.capture = false
				j.value = j.value[:0]
			}
		case ':':
			if j.depth == 1 && j.usageKey {
				j.wait = true
			}
		case ',':
			if j.depth == 1 {
				j.wantKey, j.usageKey = true, false
			}
		}
	}
}

func (t *Totals) readUsage(raw []byte, protocol string, input, output bool) bool {
	var value struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
		Input      *int64 `json:"input_tokens"`
		Output     *int64 `json:"output_tokens"`
	}
	if len(raw) > maxUsage || json.Unmarshal(raw, &value) != nil {
		return false
	}
	in, out := value.Prompt, value.Completion
	if protocol == config.ProtocolAnthropic {
		in, out = value.Input, value.Output
	}
	valid := false
	if input && in != nil && *in >= 0 {
		t.InputTokens, t.HasInput, valid = *in, true, true
	}
	if output && out != nil && *out >= 0 {
		t.OutputTokens, t.HasOutput, valid = *out, true, true
	}
	return valid
}

func space(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func trim(p []byte) []byte { return bytes.TrimSpace(p) }

// keyEquals only decodes escaped field names, which are unusual on the wire.
func keyEquals(raw []byte, name string) bool {
	if len(raw) < 2 {
		return false
	}
	if !bytes.ContainsRune(raw, '\\') {
		return bytes.Equal(raw[1:len(raw)-1], []byte(name))
	}
	var decoded string
	return json.Unmarshal(raw, &decoded) == nil && decoded == name
}

// valueEnd skips a JSON value without allocating or decoding its content.
func valueEnd(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	depth, quoted, escaped := 0, false, false
	for i, c := range p {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
				if depth == 0 {
					return i + 1
				}
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return i
			}
			depth--
			if depth == 0 {
				return i + 1
			}
		case ',', ' ', '\t', '\r', '\n':
			if depth == 0 {
				return i
			}
		}
	}
	return len(p)
}

// field returns a view of a top-level field. Ordinary content events therefore
// need no map, JSON decoder, or allocated strings.
func field(object []byte, name string) []byte {
	p := trim(object)
	if len(p) == 0 || p[0] != '{' {
		return nil
	}
	p = trim(p[1:])
	for len(p) > 0 && p[0] == '"' {
		end := valueEnd(p)
		key := p[:end]
		p = trim(p[end:])
		if len(p) == 0 || p[0] != ':' {
			return nil
		}
		p = trim(p[1:])
		end = valueEnd(p)
		if keyEquals(key, name) {
			return p[:end]
		}
		p = trim(p[end:])
		if len(p) == 0 || p[0] != ',' {
			return nil
		}
		p = trim(p[1:])
	}
	return nil
}

func nonemptyString(p []byte) bool { return len(p) > 2 && p[0] == '"' }

func arrayAny(p []byte, match func([]byte) bool) bool {
	p = trim(p)
	if len(p) == 0 || p[0] != '[' {
		return false
	}
	p = trim(p[1:])
	for len(p) > 0 && p[0] != ']' {
		end := valueEnd(p)
		if end == 0 {
			return false
		}
		if match(p[:end]) {
			return true
		}
		p = trim(p[end:])
		if len(p) == 0 || p[0] != ',' {
			return false
		}
		p = trim(p[1:])
	}
	return false
}
