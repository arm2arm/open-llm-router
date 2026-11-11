// Package metrics observes aggregate operational metadata. It never receives
// request/response bodies, headers, cookies or API keys.
package metrics

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arm2arm/open-llm-router/internal/auth"
	"github.com/arm2arm/open-llm-router/internal/pool"
	"github.com/arm2arm/open-llm-router/internal/reqid"
	"github.com/arm2arm/open-llm-router/internal/usage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type Options struct {
	Token         string
	TraceEndpoint string // full OTLP/HTTP traces URL; empty disables spans
	IncludeUserID bool
	Exporter      sdktrace.SpanExporter // optional replacement for tests
}

func Environment(lookup func(string) string) (Options, error) {
	o := Options{Token: lookup("OLR_METRICS_TOKEN"), TraceEndpoint: lookup("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")}
	if o.TraceEndpoint == "" && lookup("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		o.TraceEndpoint = strings.TrimRight(lookup("OTEL_EXPORTER_OTLP_ENDPOINT"), "/") + "/v1/traces"
	}
	if value := lookup("OLR_TRACE_USER_ID"); value != "" {
		var err error
		o.IncludeUserID, err = strconv.ParseBool(value)
		if err != nil {
			return o, errors.New("OLR_TRACE_USER_ID must be a boolean")
		}
	}
	return o, nil
}

type Observer struct {
	Registry                *prometheus.Registry
	requests, input, output *prometheus.CounterVec
	duration, ttfb, tps     *prometheus.HistogramVec
	rejections              *prometheus.CounterVec
	token                   [32]byte
	protected               bool
	provider                *sdktrace.TracerProvider
	tracer                  trace.Tracer
	includeUser             bool
}

func New(ctx context.Context, options Options, pools *pool.Manager, writer interface{ Counters() usage.Counters }) (*Observer, error) {
	o := &Observer{Registry: prometheus.NewRegistry(), token: sha256.Sum256([]byte(options.Token)), protected: options.Token != "", tracer: noop.NewTracerProvider().Tracer("olr"), includeUser: options.IncludeUserID}
	labels := []string{"model", "replica", "protocol", "status_class"}
	counter := func(name, help string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "olr_" + name, Help: help}, labels)
		o.Registry.MustRegister(c)
		return c
	}
	histogram := func(name, help string, buckets []float64) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "olr_" + name, Help: help, Buckets: buckets}, labels)
		o.Registry.MustRegister(h)
		return h
	}
	o.requests = counter("requests_total", "Completed proxy calls, including rejections and interrupted streams.")
	o.input = counter("input_tokens_total", "Observed input tokens.")
	o.output = counter("output_tokens_total", "Observed output tokens.")
	o.duration = histogram("request_duration_seconds", "Total proxy call duration.", []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 900})
	o.ttfb = histogram("time_to_first_token_seconds", "Time to first upstream body byte, including selection and request processing.", []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30})
	o.tps = histogram("output_tokens_per_second", "Stream output tokens divided by time after first upstream byte.", []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000})
	o.rejections = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "olr_limit_rejections_total", Help: "Calls rejected by a configured quota or concurrency limit."}, []string{"model", "protocol", "limit"})
	o.Registry.MustRegister(o.rejections, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	if pools != nil {
		o.Registry.MustRegister(newPoolCollector(pools))
	}
	if writer != nil {
		o.Registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "olr_usage_write_failures_total", Help: "Failed usage database transactions."}, func() float64 { return float64(writer.Counters().Failed) }), prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "olr_usage_dropped_total", Help: "Usage records dropped because the queue is full or closed."}, func() float64 { return float64(writer.Counters().Dropped) }))
	}
	if options.TraceEndpoint != "" {
		u, err := url.Parse(options.TraceEndpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("OTLP traces endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
		exporter := options.Exporter
		if exporter == nil {
			exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(options.TraceEndpoint), otlptracehttp.WithTimeout(5*time.Second))
			if err != nil {
				return nil, errors.New("initialize OTLP trace exporter failed")
			}
		}
		o.provider = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "open-llm-router"))))
		o.tracer = o.provider.Tracer("github.com/arm2arm/open-llm-router")
	}
	return o, nil
}

func (o *Observer) Shutdown(ctx context.Context) error {
	if o.provider != nil {
		return o.provider.Shutdown(ctx)
	}
	return nil
}

// Flush exports completed spans without shutting down the observer.
func (o *Observer) Flush(ctx context.Context) error {
	if o.provider != nil {
		return o.provider.ForceFlush(ctx)
	}
	return nil
}

func (o *Observer) Handler() http.Handler {
	next := promhttp.HandlerFor(o.Registry, promhttp.HandlerOpts{Timeout: 5 * time.Second})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		headers := r.Header.Values("Authorization")
		var provided string
		if len(headers) == 1 && strings.HasPrefix(headers[0], "Bearer ") {
			provided = strings.TrimPrefix(headers[0], "Bearer ")
		}
		hash := sha256.Sum256([]byte(provided))
		if !o.protected || subtle.ConstantTimeCompare(hash[:], o.token[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "metrics authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start extracts W3C trace context only. Baggage is neither recorded nor sent.
func (o *Observer) Start(r *http.Request, protocol string) (context.Context, trace.Span) {
	ctx := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := o.tracer.Start(ctx, "proxy call", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("olr.protocol", protocol), attribute.String("gen_ai.provider.name", "open_llm_router")))
	if details := reqid.DetailsFromContext(ctx); details != nil && span.SpanContext().IsValid() {
		details.TraceID = span.SpanContext().TraceID().String()
	}
	return ctx, span
}

func Inject(ctx context.Context, headers http.Header) {
	headers.Del("Traceparent")
	headers.Del("Tracestate")
	headers.Del("Baggage")
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(headers))
}

func (o *Observer) Reject(model, protocol, limit string) {
	// Limit names come from the fixed enforcement gates, never caller input.
	o.rejections.WithLabelValues(model, protocol, limit).Inc()
}

func (o *Observer) Finish(ctx context.Context, span trace.Span, r usage.Record, selection time.Duration, operation string) {
	class := "error"
	if status, err := strconv.Atoi(r.Status); err == nil && status >= 100 && status <= 599 {
		class = strconv.Itoa(status/100) + "xx"
	} else if r.Status == "aborted" {
		class = "aborted"
	}
	values := []string{r.Model, r.Replica, r.Protocol, class}
	o.requests.WithLabelValues(values...).Inc()
	o.input.WithLabelValues(values...).Add(float64(r.InputTokens))
	o.output.WithLabelValues(values...).Add(float64(r.OutputTokens))
	o.duration.WithLabelValues(values...).Observe(float64(r.LatencyMS) / 1000)
	if r.TTFBMS != nil {
		o.ttfb.WithLabelValues(values...).Observe(float64(*r.TTFBMS) / 1000)
	}
	if r.IsStream && r.OutputTokens > 0 && r.TTFBMS != nil && r.LatencyMS > *r.TTFBMS {
		o.tps.WithLabelValues(values...).Observe(r.TokensPerSecond())
	}
	attrs := []attribute.KeyValue{attribute.String("gen_ai.operation.name", operation), attribute.String("gen_ai.request.model", r.Model), attribute.String("olr.replica", r.Replica), attribute.String("olr.status", r.Status), attribute.Int64("gen_ai.usage.input_tokens", r.InputTokens), attribute.Int64("gen_ai.usage.output_tokens", r.OutputTokens), attribute.Float64("olr.replica_selection.duration", selection.Seconds()), attribute.Float64("olr.request.duration", float64(r.LatencyMS)/1000)}
	if code, err := strconv.Atoi(r.Status); err == nil {
		attrs = append(attrs, attribute.Int("http.response.status_code", code))
	}
	if r.TTFBMS != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.response.time_to_first_chunk", float64(*r.TTFBMS)/1000))
	}
	if r.IsStream {
		attrs = append(attrs, attribute.Float64("olr.output.tokens_per_second", r.TokensPerSecond()))
	}
	if principal, ok := auth.FromContext(ctx); ok {
		attrs = append(attrs, attribute.StringSlice("olr.groups", principal.Groups))
		if o.includeUser {
			attrs = append(attrs, attribute.String("enduser.id", principal.UserID))
		}
	}
	span.SetAttributes(attrs...)
	if class == "error" || class == "aborted" || class == "5xx" || class == "4xx" {
		span.SetStatus(codes.Error, "proxy call failed")
	}
	span.End()
}
