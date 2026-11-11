"""Generate the Grafana dashboard: python3 deploy/observability/tools/dashboard.py > deploy/observability/grafana/dashboards/open-llm-router.json"""

import json

DS = {"type": "prometheus", "uid": "prometheus"}
M = '{model=~"$model"}'
R = "$__rate_interval"
panels = []
y = 0


def row(title):
    global y
    panels.append({"type": "row", "title": title, "collapsed": False, "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "id": len(panels) + 1, "panels": []})
    y += 1


def panel(title, targets, unit="short", w=12, h=8, x=0, kind="timeseries", description="", extra=None):
    p = {
        "type": kind, "title": title, "description": description, "datasource": DS, "id": len(panels) + 1,
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "targets": [{"datasource": DS, "expr": e, "legendFormat": l, "refId": chr(65 + i)} for i, (e, l) in enumerate(targets)],
        "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 2, "fillOpacity": 0, "showPoints": "never"}}, "overrides": []},
        "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True}, "tooltip": {"mode": "multi", "sort": "desc"}},
    }
    if extra:
        p.update(extra)
    panels.append(p)


def next_row(h=8):
    global y
    y += h


row("Traffic")
panel("Requests per second by model and status", [(f'sum by (model, status_class) (rate(olr_requests_total{M}[{R}]))', "{{model}} {{status_class}}")], "reqps",
      description="Completed proxy calls, including rejections and interrupted streams (status_class aborted/error).")
panel("Tokens per second by model", [(f'sum by (model) (rate(olr_input_tokens_total{M}[{R}]))', "{{model}} input"),
                                    (f'sum by (model) (rate(olr_output_tokens_total{M}[{R}]))', "{{model}} output")], "short", x=12)
next_row()
row("Latency")
panel("Time to first token (p50, p95)", [(f'histogram_quantile(0.5, sum by (le, model) (rate(olr_time_to_first_token_seconds_bucket{M}[{R}])))', "{{model}} p50"),
                                         (f'histogram_quantile(0.95, sum by (le, model) (rate(olr_time_to_first_token_seconds_bucket{M}[{R}])))', "{{model}} p95")], "s", w=8)
panel("Request duration (p50, p95)", [(f'histogram_quantile(0.5, sum by (le, model) (rate(olr_request_duration_seconds_bucket{M}[{R}])))', "{{model}} p50"),
                                      (f'histogram_quantile(0.95, sum by (le, model) (rate(olr_request_duration_seconds_bucket{M}[{R}])))', "{{model}} p95")], "s", w=8, x=8)
panel("Output tokens per second per stream (p50)", [(f'histogram_quantile(0.5, sum by (le, model) (rate(olr_output_tokens_per_second_bucket{M}[{R}])))', "{{model}}")], "short", w=8, x=16,
      description="Generation speed one client sees: output tokens divided by the time after the first byte.")
next_row()
row("Pools")
panel("In flight per replica", [(f'olr_replica_in_flight{M}', "{{model}} {{replica}}")], "short", w=8)
panel("Replica health", [(f'olr_replica_healthy{M}', "{{model}} {{replica}}")], "short", w=8, x=8, kind="state-timeline",
      extra={"fieldConfig": {"defaults": {"mappings": [{"type": "value", "options": {"0": {"text": "down", "color": "red"}, "1": {"text": "healthy", "color": "green"}}}],
                                          "color": {"mode": "thresholds"}, "thresholds": {"mode": "absolute", "steps": [{"color": "red", "value": None}, {"color": "green", "value": 1}]}},
                             "overrides": []}, "options": {"showValue": "never", "legend": {"showLegend": False}}})
panel("Calls waiting for a replica", [(f'olr_queue_waiting{M}', "{{model}}")], "short", w=8, x=16,
      description="Queue of models with queue_timeout_s; calls that time out get 503.")
next_row()
row("Limits and accounting")
panel("Rejections per second by limit", [(f'sum by (limit) (rate(olr_limit_rejections_total{M}[{R}]))', "{{limit}}")], "reqps", w=12)
panel("Usage records lost", [(f'rate(olr_usage_dropped_total[{R}])', "dropped (queue full)"), (f'rate(olr_usage_write_failures_total[{R}])', "write failures")], "short", w=12, x=12,
      description="Must stay at zero: dropped or failed records mean usage and quotas undercount.")
next_row()
row("Backends (vLLM /metrics) and router process")
panel("vLLM requests running and waiting", [("sum by (instance) (vllm:num_requests_running)", "{{instance}} running"),
                                           ("sum by (instance) (vllm:num_requests_waiting)", "{{instance}} waiting")], "short", w=12,
      description="Scraped from the backends listed in vllm-targets.json; empty without them.")
panel("vLLM KV cache usage", [("avg by (instance) (vllm:kv_cache_usage_perc)", "{{instance}}")], "percentunit", w=12, x=12)
next_row()
panel("Router goroutines", [('go_goroutines{job="olr"}', "goroutines")], "short", w=12)
panel("Router resident memory", [('process_resident_memory_bytes{job="olr"}', "RSS")], "bytes", w=12, x=12)

dashboard = {
    "uid": "open-llm-router", "title": "open-llm-router", "tags": ["open-llm-router"], "timezone": "browser",
    "schemaVersion": 41, "version": 1, "editable": False, "refresh": "10s", "time": {"from": "now-1h", "to": "now"},
    "templating": {"list": [{"name": "model", "label": "Model", "type": "query", "datasource": DS, "query": {"query": "label_values(olr_requests_total, model)", "refId": "model"},
                             "definition": "label_values(olr_requests_total, model)", "includeAll": True, "multi": True, "allValue": ".*",
                             "current": {"selected": True, "text": ["All"], "value": ["$__all"]}, "refresh": 2}]},
    "panels": panels,
}
print(json.dumps(dashboard, indent=2))
