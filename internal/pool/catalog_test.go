package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

// catalogBackend serves /health and a /v1/models catalog whose body can be
// changed during a test. It counts catalog reads.
type catalogBackend struct {
	*httptest.Server
	catalog atomic.Pointer[string]
	healthy atomic.Bool
	reads   atomic.Int32
}

func newCatalogBackend(t *testing.T, catalog string) *catalogBackend {
	t.Helper()
	b := &catalogBackend{}
	b.catalog.Store(&catalog)
	b.healthy.Store(true)
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer backend-key" {
			t.Error("catalog or probe without the replica's credential", r.URL)
		}
		switch r.URL.Path {
		case "/health":
		case CatalogPath:
			b.reads.Add(1)
			_, _ = w.Write([]byte(*b.catalog.Load()))
			return
		default:
			t.Error("unexpected request", r.URL)
		}
		if !b.healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *catalogBackend) replica() config.Replica {
	return config.Replica{URL: b.URL, Weight: 1, MaxInflight: 1, BearerToken: "backend-key"}
}

func TestContextLengthIsSmallestReportedAcrossReplicas(t *testing.T) {
	large := newCatalogBackend(t, `{"object":"list","data":[{"id":"served","owned_by":"vllm","max_model_len":131072},{"id":"other","max_model_len":2048}]}`)
	small := newCatalogBackend(t, `{"data":[{"id":"served","owned_by":"vllm","max_model_len":32768}]}`)
	silent := newCatalogBackend(t, `{"data":[{"id":"served","owned_by":"ollama"}]}`)
	cfg := &config.Config{Models: map[string]config.Model{
		"logical": {UpstreamModel: "served", Pool: []config.Replica{large.replica(), small.replica(), silent.replica()}},
		"other":   {Pool: []config.Replica{large.replica()}},
		"missing": {Pool: []config.Replica{silent.replica()}},
	}}
	m := New(cfg, http.DefaultTransport)
	if m.ContextLength("logical") != 0 {
		t.Fatal("context length known before any check")
	}
	m.Check(t.Context())
	// The upstream model name is looked up, not the logical one; replicas
	// that report nothing do not lower the value.
	if got := m.ContextLength("logical"); got != 32768 {
		t.Fatalf("logical context length = %d, want the smallest reported 32768", got)
	}
	if got := m.ContextLength("other"); got != 2048 {
		t.Fatalf("other context length = %d", got)
	}
	if got := m.ContextLength("missing"); got != 0 {
		t.Fatalf("unreported context length = %d", got)
	}
	if got := m.ContextLength("unconfigured"); got != 0 {
		t.Fatalf("unknown model context length = %d", got)
	}
	var none *Manager
	if none.ContextLength("logical") != 0 {
		t.Fatal("nil manager reported a context length")
	}
	states := m.Pools["logical"].Snapshot()
	if states[0].ContextLength != 131072 || states[1].ContextLength != 32768 || states[2].ContextLength != 0 {
		t.Fatalf("replica states = %+v", states)
	}
	encoded, _ := json.Marshal(states[2])
	if strings.Contains(string(encoded), "context_length") {
		t.Fatalf("unknown replica context length encoded: %s", encoded)
	}
}

func TestCatalogIsReadWhenDue(t *testing.T) {
	b := newCatalogBackend(t, `{"data":[{"id":"m","max_model_len":8192}]}`)
	m := New(&config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{b.replica()}}}}, http.DefaultTransport)
	ctx := t.Context()
	m.Check(ctx)
	m.Check(ctx)
	if b.reads.Load() != 1 || m.ContextLength("m") != 8192 {
		t.Fatalf("reads=%d context=%d, want one catalog read for repeated healthy checks", b.reads.Load(), m.ContextLength("m"))
	}

	// After CatalogInterval the catalog is read again, and a new value wins.
	p := m.Pools["m"]
	p.mu.Lock()
	p.replicas[0].catalogAt = time.Now().Add(-CatalogInterval)
	p.mu.Unlock()
	next := `{"data":[{"id":"m","max_model_len":4096}]}`
	b.catalog.Store(&next)
	m.Check(ctx)
	if b.reads.Load() != 2 || m.ContextLength("m") != 4096 {
		t.Fatalf("reads=%d context=%d after the refresh interval", b.reads.Load(), m.ContextLength("m"))
	}

	// An unhealthy replica is not read; a recovered one is, since a restart
	// can change the model. An unreadable catalog keeps the last value.
	b.healthy.Store(false)
	m.Check(ctx)
	m.Check(ctx)
	if b.reads.Load() != 2 || m.Ready() {
		t.Fatalf("reads=%d while unhealthy", b.reads.Load())
	}
	broken := `<html>not a catalog</html>`
	b.catalog.Store(&broken)
	b.healthy.Store(true)
	m.Check(ctx)
	if b.reads.Load() != 3 || !m.Ready() || m.ContextLength("m") != 4096 {
		t.Fatalf("reads=%d context=%d after recovery with an unreadable catalog", b.reads.Load(), m.ContextLength("m"))
	}

	// A readable catalog without the model clears the value.
	p.mu.Lock()
	p.replicas[0].catalogAt = time.Time{}
	p.mu.Unlock()
	removed := `{"data":[{"id":"replacement","max_model_len":4096}]}`
	b.catalog.Store(&removed)
	m.Check(ctx)
	if b.reads.Load() != 4 || m.ContextLength("m") != 0 {
		t.Fatalf("reads=%d context=%d after the model left the catalog", b.reads.Load(), m.ContextLength("m"))
	}
}

func TestCatalogHealthPathIsReadOnce(t *testing.T) {
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != CatalogPath {
			t.Error("unexpected request", r.URL)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_length":65536}]}`))
	}))
	defer backend.Close()
	m := New(&config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: backend.URL, HealthPath: CatalogPath, Weight: 1, MaxInflight: 1}}}}}, http.DefaultTransport)
	m.Check(t.Context())
	if requests.Load() != 1 || !m.Ready() || m.ContextLength("m") != 65536 {
		t.Fatalf("requests=%d context=%d, want the probe itself to be the catalog read", requests.Load(), m.ContextLength("m"))
	}
}

func TestCatalogWithoutModelsEndpointKeepsReplicaHealthy(t *testing.T) {
	var reads atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CatalogPath {
			reads.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	m := New(&config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{{URL: backend.URL, HealthPath: "/", Weight: 1, MaxInflight: 1}}}}}, http.DefaultTransport)
	m.Check(t.Context())
	m.Check(t.Context())
	if !m.Ready() || m.ContextLength("m") != 0 || reads.Load() != 1 {
		t.Fatalf("ready=%v context=%d reads=%d", m.Ready(), m.ContextLength("m"), reads.Load())
	}
}

func TestReportedContextLength(t *testing.T) {
	for raw, want := range map[string]int{
		`{"max_model_len":32768}`:                      32768,
		`{"context_length":200000}`:                    200000,
		`{"max_model_len":4096,"context_length":8192}`: 4096,
		`{"max_model_len":null,"context_length":8192}`: 8192,
		`{"max_model_len":"32768"}`:                    0,
		`{"max_model_len":32768.5}`:                    0,
		`{"max_model_len":0}`:                          0,
		`{"max_model_len":-1}`:                         0,
		`{"max_model_len":2147483648}`:                 0,
		`{"max_model_len":{"value":1}}`:                0,
		`{"max_model_len":true}`:                       0,
		`{}`:                                           0,
	} {
		var entry CatalogEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := entry.ReportedContextLength(); got != want {
			t.Errorf("%s: %d, want %d", raw, got, want)
		}
	}
	if _, ok := catalogContextLength(strings.NewReader(`{"data":[`+strings.Repeat(" ", MaxCatalogBytes)+`]}`), "m"); ok {
		t.Fatal("oversized catalog read")
	}
}

func TestChangedUpstreamModelReplacesPool(t *testing.T) {
	replica := config.Replica{URL: "http://backend", Weight: 1, MaxInflight: 1}
	cfg := &config.Config{Models: map[string]config.Model{"m": {Pool: []config.Replica{replica}}}}
	first := New(cfg, http.DefaultTransport)
	same := Replace(cfg, http.DefaultTransport, first)
	if same.Pools["m"] != first.Pools["m"] {
		t.Fatal("unchanged pool was not reused")
	}
	renamed := &config.Config{Models: map[string]config.Model{"m": {UpstreamModel: "served", Pool: []config.Replica{replica}}}}
	next := Replace(renamed, http.DefaultTransport, first)
	if next.Pools["m"] == first.Pools["m"] || next.Pools["m"].upstream != "served" {
		t.Fatal("a pool looking up another upstream model was reused")
	}
}
