package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/metrics"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
	"github.com/arm2arm/open-llm-router/internal/proxy"
	"github.com/arm2arm/open-llm-router/internal/server"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestObservabilityMetadataAndTracePropagation(t *testing.T) {
	const marker = "private-prompt-marker"
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	entered := make(chan http.Header, 1)
	release := make(chan struct{})
	mock := mockllm.New(mockllm.Options{Tokens: 7, TokenDelay: 5 * time.Millisecond})
	exporter := tracetest.NewInMemoryExporter()
	var observer *metrics.Observer
	f := startUsageRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			entered <- r.Header.Clone()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		mock.ServeHTTP(w, r)
	}), usageOptions{configure: func(_ *config.Config, h *proxy.Handler, s *server.Server, _ *pgxpool.Pool) {
		var err error
		observer, err = metrics.New(t.Context(), metrics.Options{Token: "test-scrape-only", TraceEndpoint: "http://collector/v1/traces", Exporter: exporter}, h.Pools, nil)
		if err != nil {
			t.Fatal(err)
		}
		h.Metrics = observer
		s.Handle("GET /metrics", observer.Handler())
	}})
	t.Cleanup(func() { observer.Shutdown(t.Context()) })
	req, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[{"role":"user","content":"`+marker+`"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Traceparent", parent)
	req.Header.Set("Baggage", "private="+marker)
	done := make(chan error, 1)
	go func() {
		resp, err := f.router.Client().Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				err = io.ErrUnexpectedEOF
			}
		}
		done <- err
	}()
	var backendHeaders http.Header
	select {
	case backendHeaders = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backend not entered")
	}
	if backendHeaders.Get("Baggage") != "" {
		t.Fatal("baggage reached backend")
	}
	backendContext := propagation.TraceContext{}.Extract(t.Context(), propagation.HeaderCarrier(backendHeaders))
	backendParent := trace.SpanContextFromContext(backendContext)
	if backendParent.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || backendParent.SpanID().String() == "00f067aa0ba902b7" {
		t.Fatal("backend did not join router call span")
	}
	checkGauge := func(want float64) {
		t.Helper()
		families, err := observer.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() == "olr_replica_in_flight" {
				if family.Metric[0].GetGauge().GetValue() != want {
					t.Fatal("in-flight gauge mismatch")
				}
				return
			}
		}
		t.Fatal("missing in-flight metric")
	}
	checkGauge(1)
	close(release)
	if err := <-done; err != nil {
		t.Fatal("inference failed", err)
	}
	// The response can finish before the proxy/accounting/logging defers run.
	finishedBy := time.Now().Add(3 * time.Second)
	for !strings.Contains(f.logs.String(), `"path":"/v1/chat/completions"`) {
		if time.Now().After(finishedBy) {
			t.Fatal("request did not finish logging")
		}
		time.Sleep(time.Millisecond)
	}
	checkGauge(0)
	// Finishing the response and the handler's defer can race. Poll metadata.
	deadline := time.Now().Add(3 * time.Second)
	for {
		families, err := observer.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, family := range families {
			for _, sample := range family.Metric {
				if strings.HasPrefix(family.GetName(), "olr_") {
					for _, label := range sample.Label {
						if label.GetName() != "model" && label.GetName() != "replica" && label.GetName() != "protocol" && label.GetName() != "status_class" && label.GetName() != "limit" {
							t.Fatal("non-aggregate metric label", label.GetName())
						}
					}
				}
			}
			if family.GetName() == "olr_requests_total" {
				found = family.Metric[0].GetCounter().GetValue() == 1
			}
			if family.GetName() == "olr_input_tokens_total" && family.Metric[0].GetCounter().GetValue() != 1 {
				t.Fatal("input token metric mismatch")
			}
			if family.GetName() == "olr_output_tokens_total" && family.Metric[0].GetCounter().GetValue() != 7 {
				t.Fatal("output token metric mismatch")
			}
			if (family.GetName() == "olr_request_duration_seconds" || family.GetName() == "olr_time_to_first_token_seconds" || family.GetName() == "olr_output_tokens_per_second") && family.Metric[0].GetHistogram().GetSampleCount() != 1 {
				t.Fatal("missing timing observation")
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing request counter")
		}
		time.Sleep(time.Millisecond)
	}
	if err := observer.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatal("expected one call span")
	}
	if spans[0].Parent.SpanID().String() != "00f067aa0ba902b7" || spans[0].SpanContext.SpanID() != backendParent.SpanID() {
		t.Fatal("wrong trace parent")
	}
	families, _ := observer.Registry.Gather()
	encoded, _ := json.Marshal(families)
	traceJSON, _ := json.Marshal(spans)
	for _, secret := range []string{marker, f.key, f.owner} {
		if strings.Contains(string(encoded), secret) || strings.Contains(string(traceJSON), secret) {
			t.Fatal("private metadata entered aggregate metrics or default traces")
		}
	}
	logs := f.logs.String()
	if strings.Contains(logs, marker) || strings.Contains(logs, f.key) {
		t.Fatal("request log contains content or full key")
	}
	var requestLog map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["path"] == "/v1/chat/completions" {
			if requestLog != nil {
				t.Fatal("multiple log lines for one request")
			}
			requestLog = entry
		}
	}
	if requestLog["user_id"] != f.owner || requestLog["key_prefix"] != f.key[:12] || requestLog["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || requestLog["model"] != "chat-model" || requestLog["input_tokens"] != float64(1) || requestLog["output_tokens"] != float64(7) {
		t.Fatal("request log missing metadata")
	}
}

func TestLimitRejectionsReachMetrics(t *testing.T) {
	rpm := 1
	var observer *metrics.Observer
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 1}), usageOptions{group: &config.Group{Models: []string{"*"}, RPM: &rpm}, configure: func(_ *config.Config, h *proxy.Handler, _ *server.Server, _ *pgxpool.Pool) {
		var err error
		observer, err = metrics.New(t.Context(), metrics.Options{}, h.Pools, nil)
		if err != nil {
			t.Fatal(err)
		}
		h.Metrics = observer
	}})
	f.request(t, "/v1/chat/completions", "")
	r, _ := http.NewRequestWithContext(t.Context(), "POST", f.router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	r.Header.Set("Authorization", "Bearer "+f.key)
	resp, err := f.router.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatal("expected limit rejection")
	}
	families, err := observer.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "olr_limit_rejections_total" {
			if len(family.Metric) != 1 || family.Metric[0].GetCounter().GetValue() != 1 {
				t.Fatal("wrong rejection count")
			}
			labels := map[string]string{}
			for _, label := range family.Metric[0].Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["limit"] != "rpm" || labels["model"] != "chat-model" || labels["protocol"] != "openai" {
				t.Fatal("wrong rejection labels")
			}
			return
		}
	}
	t.Fatal("limit rejection was not observed")
}
