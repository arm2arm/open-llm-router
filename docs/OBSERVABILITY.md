# Metrics, traces and request logs

## Protected Prometheus endpoint

Set a dedicated secret `OLR_METRICS_TOKEN` in the router environment. `GET /metrics`
on the normal HTTP port requires exactly one `Authorization: Bearer <token>` header.
API keys and portal cookies do not authorize it. With an empty setting, every scrape
is refused with 401. Configure Prometheus's `authorization.credentials_file` with
this token and use HTTPS outside local development. Never put tokens in query strings.

```yaml
scrape_configs:
  - job_name: olr
    scheme: https
    static_configs:
      - targets: [router.example.org]
    authorization:
      credentials_file: /run/secrets/olr_metrics_token
```

Counters and histograms describe authenticated proxy calls, including local model/
policy/limit rejections. They do not count portal, health, model-list or usage API calls.

| Metric | Type / meaning |
|---|---|
| `olr_requests_total` | completed calls, once per client call including failover |
| `olr_input_tokens_total`, `olr_output_tokens_total` | observed protocol usage |
| `olr_request_duration_seconds` | histogram of total proxy latency |
| `olr_time_to_first_token_seconds` | histogram of time to first upstream body byte |
| `olr_output_tokens_per_second` | histogram of measured streaming throughput |
| `olr_replica_in_flight` | gauge of current replica leases, including failed attempts until release |
| `olr_replica_healthy` | gauge: 1 for healthy/outside cooldown, otherwise 0 |
| `olr_limit_rejections_total` | rejections by `max_concurrent`, `rpm`, `tokens_per_day`, `tokens_per_month` |
| `olr_usage_write_failures_total`, `olr_usage_dropped_total` | failed database transactions and dropped queued records |

Call metrics have only canonical configured `model`, configured replica origin,
`protocol` and `status_class` labels. Rejections have model/protocol/limit; pool gauges
have model/replica. Invalid model requests use an empty model label, preventing caller-
controlled cardinality. Status classes include `2xx`–`5xx`, `error` and `aborted`.
There are no user, key, group, request-ID or trace-ID metric labels. Standard Go and
process collectors are included. Counters reset on process restart.

First-byte timing is the same TTFB approximation stored in usage; it can include a
protocol role/start event before generated text. Throughput is output tokens divided
by latency after that byte, for streams with positive output and duration. Missing
measurements are not histogram observations. Usage accounting remains asynchronous;
metrics record the observed call even when its database write fails.

## Optional OTLP/HTTP tracing

No tracing SDK provider/exporter is created unless an endpoint is set:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.org:4318
# Or an exact URL, taking precedence over the generic endpoint:
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://collector.example.org/v1/traces
# Optional standard exporter headers (URL-encoded values):
export OTEL_EXPORTER_OTLP_HEADERS='Authorization=Bearer%20collector-token'
```

The exporter sends HTTP/protobuf, not gRPC. The generic endpoint gets `/v1/traces`
appended; the traces-specific URL is used as-is. HTTPS is recommended; HTTP supports
internal/local collectors. Credentials in endpoint URLs, queries and fragments are
rejected. Standard OTLP headers, timeout/retry behavior and certificate settings are
provided by the [OpenTelemetry Go exporter](https://opentelemetry.io/docs/languages/go/exporters/).
The router sets a five-second export timeout and flushes during graceful shutdown.
Compose passes these settings to the router; use a collector hostname reachable
from its container.

Each proxy call creates one client span, including calls rejected before an upstream
is selected. Failover stays within that span. W3C `traceparent`/`tracestate` are extracted,
the call retains the incoming trace ID, and the router span context is injected into
the backend request. Baggage is discarded. With tracing disabled, valid incoming
trace context can still propagate, but no new spans are recorded or exported.

Spans use the [GenAI attribute names](https://opentelemetry.io/docs/specs/semconv/registry/attributes/gen-ai/)
where defined: `gen_ai.provider.name`, `gen_ai.operation.name`, `gen_ai.request.model`,
`gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` and
`gen_ai.response.time_to_first_chunk`. `http.response.status_code` is included for
numeric outcomes. Router-specific fields are `olr.protocol`, `olr.replica`, `olr.status`,
`olr.request.duration`, `olr.replica_selection.duration` (cumulative selection time in
seconds), and `olr.output.tokens_per_second` for streams. Groups are `olr.groups`.
The service name is `open-llm-router`. No prompt, response, request headers or keys
are passed to tracing. `OLR_TRACE_USER_ID=true` explicitly adds `enduser.id`;
it is false by default. It never adds user IDs to metric labels.

## JSON request logs

One `msg=request` JSON line per request contains request ID, trace ID when available,
authenticated user ID and display-only key prefix, canonical model, replica, protocol,
HTTP status and accounting outcome, input/output tokens, nullable TTFB, and latency.
Probes are omitted when successful. Non-proxy requests have empty inference fields.
These logs intentionally contain user IDs; protect them according to the site's
retention policy. They contain no full keys, cookies, OAuth codes, query strings,
prompts or responses. Panic values are replaced with a generic marker.

Tests verify metric changes, nonzero/zero replica leases, trace propagation with an
in-memory exporter, default/opt-in user attributes and a prompt privacy marker across
spans, logs and metric labels.
