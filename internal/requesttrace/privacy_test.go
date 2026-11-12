package requesttrace

import (
	"github.com/prometheus/client_golang/prometheus"
	"strings"
	"testing"
	"time"
)

func TestGlobalGatePreventsContentCopies(t *testing.T) {
	s := &Service{slots: make(chan struct{}, Slots), queue: make(chan *Capture, Slots)}
	prefs := map[string]Preferences{"consented": {Enabled: true, RetentionDays: 7}}
	s.prefs.Store(&prefs)
	if s.Begin("consented", "chat", time.Now()) != nil {
		t.Fatal("default gate enabled")
	}
	s.SetEnabled(true)
	if s.Begin("new-user", "chat", time.Now()) != nil {
		t.Fatal("new user implicitly opted in")
	}
	c := s.Begin("consented", "chat", time.Now())
	if c == nil {
		t.Fatal("explicit opt-in ignored")
	}
	s.SetEnabled(false)
	c.Input([]byte("private input"))
	c.Write([]byte("private output"))
	if len(c.input) != 0 || len(c.output) != 0 {
		t.Fatal("content copied after disable")
	}
	s.SetEnabled(true)
	c.Input([]byte("private input"))
	c.Write([]byte("private output"))
	if len(c.input) != 0 || len(c.output) != 0 {
		t.Fatal("stale capture resurrected")
	}
}
func TestSupplementalRedactionPreservesJSON(t *testing.T) {
	r, err := newRedactor([]string{`ORG-[0-9]{6}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.marshal(map[string]any{"text": "Bearer secret-token password=hidden ORG-123456 sk-12345678901234567890", "nested": []any{map[string]any{"preview": `{"api_key":"never-store"}`}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token", "hidden", "ORG-123456", "12345678901234567890", "never-store"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("redaction missed %s", secret)
		}
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatal("missing redaction")
	}
}

func TestTraceMetricsAreContentFree(t *testing.T) {
	s := &Service{}
	s.Written.Add(2)
	s.Failed.Add(1)
	s.Deleted.Add(3)
	s.RetentionBacklog.Store(4)
	reg := prometheus.NewRegistry()
	s.RegisterMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 5 {
		t.Fatal("missing metrics", len(families))
	}
	for _, family := range families {
		for _, m := range family.Metric {
			if len(m.Label) != 0 {
				t.Fatal("content-bearing labels")
			}
		}
	}
}
