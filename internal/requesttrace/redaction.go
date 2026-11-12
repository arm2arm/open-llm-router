package requesttrace

import (
	"encoding/json"
	"errors"
	"regexp"

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
	if err = json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case string:
			for _, p := range r.patterns {
				x = p.ReplaceAllString(x, "[REDACTED]")
			}
			return x
		case []any:
			for i := range x {
				x[i] = clean(x[i])
			}
		case map[string]any:
			for k, v := range x {
				x[k] = clean(v)
			}
		}
		return v
	}
	return json.Marshal(clean(decoded))
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
		{"dropped_total", "Traces skipped for lack of capacity.", func() float64 { return float64(s.Dropped.Load()) }},
	} {
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "olr_content_trace_" + c.name, Help: c.help}, c.value))
	}
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "olr_content_trace_retention_backlog", Help: "Expired traces awaiting deletion at the last successful retention pass."}, func() float64 { return float64(s.RetentionBacklog.Load()) }))
}
