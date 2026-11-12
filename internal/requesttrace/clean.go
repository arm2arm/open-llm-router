package requesttrace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Raw previews show the exact JSON shape a client sent or a provider returned,
// minus anything the content policy excludes: binary payloads, attachment URLs,
// provider signatures/encrypted reasoning, arbitrary metadata and vectors. Long
// strings are shortened; their full text is in the normalized conversation.
const (
	rawStringCap = 2 << 10
	rawMaxDepth  = 32
	rawNumberCap = 32
)

var omittedKeys = map[string]string{
	"metadata":          "metadata omitted",
	"provider_metadata": "metadata omitted",
	"signature":         "signature omitted",
	"encrypted_content": "encrypted content omitted",
	"embedding":         "vector omitted",
	"b64_json":          "binary omitted",
}

var payloadTypes = map[string]string{
	"image": "image", "image_url": "image", "input_image": "image",
	"document": "document", "file": "document", "input_file": "document",
	"audio": "audio", "input_audio": "audio",
	"redacted_thinking": "redacted reasoning",
}

func marker(s string) string { return "«" + s + "»" }

func looksBinary(s string) bool {
	if len(s) < 256 || strings.ContainsAny(s, " \n\t") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func cut(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

func cleanString(s string, limit int) string {
	switch {
	case strings.HasPrefix(s, "data:") && len(s) > 64:
		return marker(fmt.Sprintf("data URI omitted, %d bytes", len(s)))
	case looksBinary(s):
		return marker(fmt.Sprintf("binary omitted, %d bytes", len(s)))
	}
	if short, cutOff := cut(s, limit); cutOff {
		return short + "…" + marker(fmt.Sprintf("+%d chars", utf8.RuneCountInString(s[len(short):])))
	}
	return s
}

// clean returns a sanitized copy of a decoded JSON value.
func clean(v any, limit, depth int) any {
	if depth > rawMaxDepth {
		return marker("nested value omitted")
	}
	switch t := v.(type) {
	case string:
		return cleanString(t, limit)
	case []any:
		numbers := len(t) > rawNumberCap
		for _, item := range t {
			if _, ok := item.(float64); !ok {
				numbers = false
				break
			}
		}
		if numbers {
			return marker(fmt.Sprintf("%d numbers omitted", len(t)))
		}
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = clean(item, limit, depth+1)
		}
		return out
	case map[string]any:
		if kind, ok := payloadTypes[str(t["type"])]; ok {
			return map[string]any{"type": t["type"], "omitted": marker(kind + " payload omitted")}
		}
		out := make(map[string]any, len(t))
		for k, item := range t {
			if why, ok := omittedKeys[k]; ok {
				out[k] = marker(why)
				continue
			}
			out[k] = clean(item, limit, depth+1)
		}
		return out
	}
	return v
}

// cleanRaw sanitizes and bounds a JSON value for storage. It returns nil when
// even aggressively shortened strings cannot fit, never a cut document.
func cleanRaw(v any, max int) json.RawMessage {
	if v == nil {
		return nil
	}
	for _, limit := range []int{rawStringCap, 256, 64} {
		b, err := json.Marshal(clean(v, limit, 0))
		if err == nil && len(b) <= max {
			return b
		}
	}
	return nil
}

// numbers flattens a usage object to dotted numeric fields only.
func numbers(v any, prefix string, out map[string]float64) {
	switch t := v.(type) {
	case float64:
		if prefix != "" {
			out[prefix] = t
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			name := k
			if prefix != "" {
				name = prefix + "." + k
			}
			numbers(t[k], name, out)
		}
	}
}
