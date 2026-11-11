package loadtest

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func TestBothProtocolsCompleteAndPropagateDisconnects(t *testing.T) {
	mock := mockllm.New(mockllm.Options{Tokens: 32, TokenDelay: 10 * time.Millisecond})
	backend := httptest.NewServer(mock)
	defer backend.Close()
	o := Options{Tokens: 32, Timeout: 3 * time.Second, Mock: true, AbortAfter: 2}
	client := newClient(4, time.Second)
	defer client.CloseIdleConnections()
	for _, protocol := range []string{"openai", "anthropic"} {
		complete := request(context.Background(), client, o, backend.URL, "chat-model", "", protocol, false)
		if complete.err != "" || complete.aborted || complete.tokens != 32 || complete.ttft <= 0 || complete.tps <= 0 {
			t.Fatalf("%s: %+v", protocol, complete)
		}
		aborted := request(context.Background(), client, o, backend.URL, "chat-model", "", protocol, true)
		if !aborted.aborted || aborted.err != "" {
			t.Fatalf("%s disconnect: %+v", protocol, aborted)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s := mock.Stats(); s.StreamsAborted == 2 && s.Inflight == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("disconnects did not reach backend: %+v", mock.Stats())
}

func TestStreamFailuresAreNotSuccessfulOrIntentionalAborts(t *testing.T) {
	for _, tc := range []struct{ name, protocol, body string }{
		{"truncated", "openai", "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"},
		{"missing usage", "openai", "data: [DONE]\n\n"},
		{"invalid JSON", "openai", "data: not json\n\n"},
		{"OpenAI error", "openai", "data: {\"error\":{\"message\":\"private response\"}}\n\n"},
		{"Anthropic error", "anthropic", "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"private response\"}}\n\n"},
		{"Anthropic missing usage", "anthropic", "data: {\"type\":\"message_stop\"}\n\n"},
		{"oversized event", "openai", "data: " + strings.Repeat("x", 1<<20) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := readStream(strings.NewReader(tc.body), tc.protocol, time.Now(), true, 4)
			if s.err == "" || s.aborted || strings.Contains(s.err, "private response") {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestHTTPFailuresDoNotDiscloseResponseBodies(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "private response and credentials")
	}))
	defer backend.Close()
	client := newClient(1, time.Second)
	defer client.CloseIdleConnections()
	s := request(context.Background(), client, Options{Timeout: time.Second, Tokens: 8}, backend.URL, "chat-model", "secret", "openai", false)
	if s.err != "HTTP 401" {
		t.Fatalf("%+v", s)
	}
}

func TestSSECommentsMultilineJSONAndCRLF(t *testing.T) {
	body := ": keep alive\r\n\r\ndata: {\r\ndata: \"choices\":[{\"delta\":{\"content\":\"one\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\r\n\r\n" +
		"data: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\r\n\r\ndata: [DONE]\r\n\r\n"
	s := readStream(strings.NewReader(body), "openai", time.Now(), false, 1)
	if s.err != "" || s.tokens != 2 {
		t.Fatalf("%+v", s)
	}
}

func validOptions() Options {
	return Options{Router: "http://router", Direct: []string{"http://a", "http://b"}, Weights: []float64{2, 1}, Model: "chat-model", APIKey: "secret", MetricsToken: "metrics-secret", Concurrency: 4, Requests: 8, Runs: 3, Tokens: 8, Timeout: time.Second, AbortAfter: 2, AbortFraction: .25, MaxAddedP95MS: 5, MaxTPSLoss: .02}
}

func TestRejectInvalidComparisonAndUnboundedOptions(t *testing.T) {
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Direct[0] = o.Router },
		func(o *Options) { o.Direct[1] = "http://a/" },
		func(o *Options) { o.Router = "http://user:password@router" },
		func(o *Options) { o.Direct[0] = "http://a/v1" },
		func(o *Options) { o.Weights[0] = math.Inf(1) },
		func(o *Options) { o.AbortFraction = math.NaN() },
		func(o *Options) { o.Requests = 1 },
		func(o *Options) { o.Timeout = 0 },
		func(o *Options) { o.APIKey = "" },
	} {
		o := validOptions()
		mutate(&o)
		if o.Validate() == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if err := validOptions().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSummarySeparatesCancelledAndFailedStreams(t *testing.T) {
	s := summarize([]sample{{ttft: 1, tps: 100, tokens: 10}, {ttft: 3, tps: 200, tokens: 10}, {aborted: true, ttft: 900}, {err: "HTTP 503", ttft: 999}}, time.Second)
	if s.Completed != 2 || s.Aborted != 1 || s.Errors["HTTP 503"] != 1 || s.P95TTFTMS != 3 || s.AggregateTPS != 20 || s.MedianTPS != 100 {
		t.Fatalf("%+v", s)
	}
}

func TestFailedPreflightCannotReportSuccessOrStartLoad(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.URL.Path != "/metrics" {
			t.Error("load started before metrics preflight")
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "secret credentials")
	}))
	defer server.Close()
	o := validOptions()
	o.Router = server.URL
	report, err := Run(context.Background(), o)
	if err == nil || report.Passed || requests.Load() != 1 || strings.Contains(err.Error(), "secret credentials") {
		t.Fatalf("requests=%d report=%+v err=%v", requests.Load(), report, err)
	}
}
