package requesttrace

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

type redactor struct{ patterns []*regexp.Regexp }

func newRedactor(extra []string) (*redactor, error) {
	if len(extra) > 32 {
		return nil, errors.New("too many trace redaction patterns")
	}
	patterns := append([]string{
		`(?i)bearer\s+[a-z0-9._~+/-]+=*`,
		`\b(?:olr-|sk-)[A-Za-z0-9_-]{16,}\b`,
		`(?i)(?:password|api[_-]?key|access[_-]?token|secret)["']?\s*[:=]\s*["']?[^\s"',;}]+`,
	}, extra...)
	r := &redactor{}
	for _, pattern := range patterns {
		if len(pattern) > 1024 {
			return nil, errors.New("trace redaction pattern too long")
		}
		p, err := regexp.Compile(pattern)
		if err != nil {
			return nil, errors.New("invalid trace redaction pattern")
		}
		r.patterns = append(r.patterns, p)
	}
	return r, nil
}
func (r *redactor) marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err = decodeJSON(data, &decoded); err != nil {
		return nil, err
	}
	value := r.value(decoded, 0)
	// Redaction can expand short credential values. Enforce the final budget
	// after sanitization as well, including on historical trace reads.
	switch v.(type) {
	case Detail, *Detail:
		root := object(value)
		parameters, limited := boundParameters(object(root["parameters"]))
		root["parameters"] = parameters
		if limited {
			root["truncated"] = true
			object(root["summary"])["truncated"] = true
			root["warnings"] = append(list(root["warnings"]), "Some parameters omitted: above the 32 KiB parameters limit after sanitization.")
		}
	}
	return json.Marshal(value)
}

func credentialField(key string) bool {
	key = strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(key))
	return key == "password" || key == "apikey" || key == "accesstoken" || key == "secret" || key == "clientsecret" || key == "authorization" || key == "xapikey"
}

func (r *redactor) text(value string) string {
	// PostgreSQL jsonb rejects NUL even when encoded as a JSON escape.
	value = strings.ReplaceAll(value, "\x00", "�")
	for _, p := range r.patterns {
		value = p.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}

func (r *redactor) value(v any, depth int) any {
	if depth > 64 {
		return marker("nested value omitted")
	}
	switch x := v.(type) {
	case string:
		// Tool arguments are JSON encoded inside a JSON string. Redact their
		// structure before re-encoding, preserving valid JSON and exact numbers.
		trimmed := strings.TrimSpace(x)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if decodeJSON([]byte(trimmed), &nested) == nil {
				data, err := json.Marshal(r.value(clean(nested, MaxInputText, 0), depth+1))
				if err == nil {
					return string(data)
				}
			}
		}
		return r.text(payloadText(x))
	case []any:
		for i := range x {
			x[i] = r.value(x[i], depth+1)
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, item := range x {
			name := r.text(key)
			if credentialField(key) {
				out[name] = "[REDACTED]"
			} else {
				out[name] = r.value(item, depth+1)
			}
		}
		return out
	}
	return v
}

// Stream previews retain numeric metadata and protocol shape. Free-text
// fragments are shown only after reconstruction in the redacted conversation.
func streamPreview(v any) any {
	switch x := v.(type) {
	case string:
		return marker("stream text omitted")
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = streamPreview(x[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, item := range x {
			// Only fixed protocol vocabulary can bypass text omission.
			if text, ok := item.(string); ok && (key == "type" || key == "role" || key == "finish_reason" || key == "stop_reason") && safeStreamWord(text) {
				out[key] = text
			} else {
				out[key] = streamPreview(item)
			}
		}
		return out
	}
	return v
}

func safeStreamWord(s string) bool {
	switch s {
	case "assistant", "user", "system", "tool", "text", "text_delta", "thinking", "thinking_delta", "input_json_delta", "tool_use", "tool_result", "function_call", "message", "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "stop", "length", "tool_calls", "end_turn", "max_tokens", "stop_sequence", "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed", "response.output_item.added", "response.output_item.done", "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return true
	}
	return false
}

// SanitizeDetail applies the current policy on reads too, so previously stored
// traces cannot expose fragments or credential fields fixed by a later release.
// It does not change retention or invent precision lost by an earlier writer.
func (s *Service) SanitizeDetail(data []byte) ([]byte, error) {
	var detail Detail
	if err := decodeJSON(data, &detail); err != nil {
		return nil, err
	}
	for i := range detail.Events {
		switch detail.Events[i].Type {
		case "text", "reasoning", "refusal", "tool_call":
			detail.Events[i].Text = ""
		}
	}
	for i := range detail.Frames {
		var value any
		if err := decodeJSON(detail.Frames[i].Data, &value); err != nil {
			return nil, err
		}
		if text, ok := value.(string); ok && text == "[DONE]" {
			continue
		}
		detail.Frames[i].Data = cleanRaw(streamPreview(value), 4<<10)
	}
	detail.Version = DetailVersion
	var r *redactor
	if s != nil {
		r = s.redactor
	}
	if r == nil {
		r, _ = newRedactor(nil)
	}
	return r.marshal(detail)
}

// RegisterMetrics uses no user, request, model, or content labels.
func (s *Service) RegisterMetrics(reg prometheus.Registerer) {
	for _, c := range []struct {
		name, help string
		value      func() float64
	}{
		{"writes_total", "Persisted content traces.", func() float64 { return float64(s.Written.Load()) }},
		{"failures_total", "Failed trace writes or retention operations.", func() float64 { return float64(s.Failed.Load()) }},
		{"deletions_total", "Traces deleted by retention or user erasure.", func() float64 { return float64(s.Deleted.Load()) }},
		{"dropped_total", "Traces skipped by capacity, rate, retained-storage or changed preference limits.", func() float64 { return float64(s.Dropped.Load()) }},
	} {
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "olr_content_trace_" + c.name, Help: c.help}, c.value))
	}
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "olr_content_trace_retention_backlog", Help: "Expired traces awaiting deletion at the last successful retention pass."}, func() float64 { return float64(s.RetentionBacklog.Load()) }))
}
