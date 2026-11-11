package metrics

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

type writerCounters struct{}

func (writerCounters) Counters() usage.Counters { return usage.Counters{Failed: 2, Dropped: 3} }

func TestEnvironment(t *testing.T) {
	for _, tc := range []struct {
		env      map[string]string
		endpoint string
		invalid  bool
	}{
		{map[string]string{}, "", false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318/prefix/"}, "http://collector:4318/prefix/v1/traces", false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://ignored", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://collector/traces"}, "https://collector/traces", false},
		{map[string]string{"OLR_TRACE_USER_ID": "invalid"}, "", true},
	} {
		o, err := Environment(func(k string) string { return tc.env[k] })
		if (err != nil) != tc.invalid || o.TraceEndpoint != tc.endpoint {
			t.Fatal("environment configuration mismatch")
		}
	}
	if _, err := New(t.Context(), Options{TraceEndpoint: "https://name:secret@collector/traces"}, nil, nil); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid endpoint must be rejected without its credentials")
	}
}

func TestOTLPHTTPExport(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "")
	payload := make(chan []byte, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			w.WriteHeader(400)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		payload <- data
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer collector.Close()
	o, err := New(t.Context(), Options{TraceEndpoint: collector.URL + "/v1/traces"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Shutdown(t.Context())
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("private-prompt-marker"))
	ctx, span := o.Start(r, "openai")
	o.Finish(ctx, span, usage.Record{Model: "chat-model", Protocol: "openai", Status: "200"}, 0, "chat")
	if err := o.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-payload:
		var request collectortrace.ExportTraceServiceRequest
		if proto.Unmarshal(data, &request) != nil || len(request.ResourceSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans) != 1 || len(request.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
			t.Fatal("invalid OTLP trace export")
		}
		if bytes.Contains(data, []byte("private-prompt-marker")) {
			t.Fatal("OTLP export contains content")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing OTLP export")
	}
}

func TestMetricsProtectionAndWriterFailures(t *testing.T) {
	o, err := New(t.Context(), Options{Token: "scrape-test-only"}, nil, writerCounters{})
	if err != nil {
		t.Fatal(err)
	}
	for _, headers := range [][]string{nil, {"Bearer wrong"}, {"Bearer scrape-test-only", "Bearer scrape-test-only"}, {"Bearer scrape-test-only"}} {
		r := httptest.NewRequest("GET", "/metrics", nil)
		for _, v := range headers {
			r.Header.Add("Authorization", v)
		}
		w := httptest.NewRecorder()
		o.Handler().ServeHTTP(w, r)
		want := 401
		if len(headers) == 1 && headers[0] == "Bearer scrape-test-only" {
			want = 200
		}
		if w.Code != want {
			t.Fatal("metrics protection", w.Code)
		}
		if want == 200 && (!strings.Contains(w.Body.String(), "olr_usage_write_failures_total 2") || !strings.Contains(w.Body.String(), "olr_usage_dropped_total 3") || !strings.Contains(w.Body.String(), "go_goroutines")) {
			t.Fatal("missing counters or Go runtime metrics")
		}
	}
	disabled, _ := New(t.Context(), Options{}, nil, nil)
	w := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 401 {
		t.Fatal("empty token must deny metrics")
	}
}

func TestTracingIsOptInAndUserIdentityIsOptional(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, includeUser := range []bool{false, true} {
			exporter := tracetest.NewInMemoryExporter()
			opts := Options{Exporter: exporter, IncludeUserID: includeUser}
			if enabled {
				opts.TraceEndpoint = "http://collector/v1/traces"
			}
			o, err := New(t.Context(), opts, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx := auth.NewContext(t.Context(), auth.Principal{UserID: "private-user", Groups: []string{"staff"}})
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("private-prompt-marker")).WithContext(ctx)
			ctx, span := o.Start(r, "openai")
			ttfb := int64(10)
			o.Finish(ctx, span, usage.Record{Model: "chat-model", Replica: "http://replica", Protocol: "openai", Status: "200", InputTokens: 3, OutputTokens: 7, LatencyMS: 100, TTFBMS: &ttfb, IsStream: true}, time.Millisecond, "chat")
			if o.provider != nil {
				if err := o.provider.ForceFlush(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			spans := exporter.GetSpans()
			if !enabled && len(spans) != 0 {
				t.Fatal("disabled tracing created spans")
			}
			if enabled {
				if len(spans) != 1 {
					t.Fatal("expected exactly one call span")
				}
				attributes := map[string]any{}
				for _, a := range spans[0].Attributes {
					attributes[string(a.Key)] = a.Value.AsInterface()
				}
				for _, key := range []string{"gen_ai.request.model", "gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.response.time_to_first_chunk", "olr.output.tokens_per_second", "olr.replica_selection.duration", "http.response.status_code", "olr.groups"} {
					if _, ok := attributes[key]; !ok {
						t.Fatal("missing span attribute", key)
					}
				}
				_, hasUser := attributes["enduser.id"]
				if hasUser != includeUser {
					t.Fatal("user identity opt-in ignored")
				}
				encoded, _ := json.Marshal(spans)
				if strings.Contains(string(encoded), "private-prompt-marker") {
					t.Fatal("trace contains request content")
				}
			}
			o.Shutdown(t.Context())
		}
	}
}
