"""Shared long-stream settings and backend cancellation evidence (stdlib only)."""

import json
import math
import os
import re
import statistics
import time
from pathlib import Path
from urllib.request import Request, urlopen


LONG_PROMPT = (
    "Write a detailed, uninterrupted essay of at least 2000 words explaining how rain forms, "
    "including cloud formation, condensation, air currents, and the water cycle. "
    "Use full sentences throughout and continue until you have covered every topic."
)


def positive_int(name, default):
    value = int(os.environ.get(name, default))
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


def long_limits():
    maximum = positive_int("OLR_LONG_MAX_TOKENS", 512)
    minimum = positive_int("OLR_LONG_MIN_TOKENS", 128)
    if minimum > maximum:
        raise ValueError("OLR_LONG_MIN_TOKENS exceeds OLR_LONG_MAX_TOKENS")
    return minimum, maximum


def mock_headers(tokens):
    # Only opt-in mock runs send these; real backends see ordinary SDK requests.
    if os.environ.get("OLR_TEST_MOCK") == "1":
        return {"X-Mock-Tokens": str(tokens), "X-Mock-Token-Delay": "2ms"}
    return {}


def comparison(samples, max_ttft_ms=5, max_tps_loss=0.02):
    medians = {
        route: {key: statistics.median(s[key] for s in runs) for key in runs[0]}
        for route, runs in samples.items()
    }
    ttft_added = medians["router"]["ttft_ms"] - medians["direct"]["ttft_ms"]
    tps_loss = 1 - medians["router"]["tokens_per_second"] / medians["direct"]["tokens_per_second"]
    return {
        "median": medians,
        "ttft_added_ms": ttft_added,
        "tps_loss_fraction": tps_loss,
        "limits": {"ttft_added_ms": max_ttft_ms, "tps_loss_fraction": max_tps_loss},
        "passed": ttft_added < max_ttft_ms and (
            medians["router"]["tokens_per_second"] >= medians["direct"]["tokens_per_second"] * (1 - max_tps_loss)
        ),
    }


def vllm_snapshot(payload, model):
    """Sum workers' gauges/counters for one served model, rejecting missing metrics."""
    names = {
        "vllm:num_requests_running": "running",
        "vllm:num_requests_waiting": "waiting",
        "vllm:request_success_total": "aborted",
    }
    values = {}
    for line in payload.splitlines():
        match = re.fullmatch(r'([^\s{]+)\{(.*)\}\s+(\S+)(?:\s+\S+)?', line)
        if not match or match[1] not in names:
            continue
        labels = dict(re.findall(r'(\w+)="((?:[^"\\]|\\.)*)"', match[2]))
        if labels.get("model_name") != model:
            continue
        if match[1] == "vllm:request_success_total" and labels.get("finished_reason") != "abort":
            continue
        value = float(match[3])
        if not math.isfinite(value) or value < 0:
            raise ValueError("invalid vLLM metric value")
        key = names[match[1]]
        values[key] = values.get(key, 0) + value
    if values.keys() != {"running", "waiting", "aborted"}:
        raise ValueError("missing vLLM running/waiting/abort metrics for OLR_UPSTREAM_MODEL")
    return values["aborted"], values["running"] + values["waiting"]


class CancellationProbe:
    """Use an idle, single-replica backend so an abort can be attributed to this test."""

    def __init__(self):
        self.mock_urls = [url for url in os.environ.get("OLR_CANCEL_STATS_URLS", "").split(",") if url]
        self.metrics_url = os.environ.get("OLR_VLLM_METRICS_URL")
        self.log_path = os.environ.get("OLR_VLLM_LOG_PATH")
        if self.mock_urls and self.metrics_url:
            raise ValueError("set only one of OLR_CANCEL_STATS_URLS and OLR_VLLM_METRICS_URL")
        if self.log_path and not self.metrics_url:
            raise ValueError("OLR_VLLM_LOG_PATH also requires OLR_VLLM_METRICS_URL")
        if self.log_path:
            self.log_path = Path(self.log_path)
            with self.log_path.open("rb") as log:
                self.log_offset = log.seek(0, 2)
            self.log_identity = self.log_path.stat().st_ino
        self.model = os.environ.get("OLR_UPSTREAM_MODEL", "")
        if self.metrics_url and not self.model:
            raise ValueError("OLR_UPSTREAM_MODEL is required for vLLM cancellation evidence")
        self.before = None
        if self.mock_urls or self.metrics_url:
            self.before, inflight = self.snapshot()
            if inflight != 0:
                raise RuntimeError("cancellation evidence requires an idle backend")
        elif os.environ.get("OLR_REQUIRE_CANCEL_EVIDENCE") == "1":
            raise ValueError("backend cancellation evidence URL is required")

    def snapshot(self):
        headers = {}
        if key := os.environ.get("OLR_DIRECT_API_KEY"):
            headers["Authorization"] = "Bearer " + key
        aborted, inflight = 0, 0
        for url in self.mock_urls or [self.metrics_url]:
            with urlopen(Request(url, headers=headers), timeout=5) as response:
                payload = response.read().decode()
            if self.mock_urls:
                stats = json.loads(payload)
                count, active = stats["streams_aborted"], stats["inflight"]
            else:
                count, active = vllm_snapshot(payload, self.model)
            aborted += count
            inflight += active
        return aborted, inflight

    def logged_abort(self, request_id):
        stat = self.log_path.stat()
        if stat.st_ino != self.log_identity or stat.st_size < self.log_offset:
            raise RuntimeError("backend log was replaced or truncated during cancellation check")
        with self.log_path.open("rb") as log:
            log.seek(self.log_offset)
            text = log.read().decode("utf-8", errors="replace")
        # vLLM can append a child-request suffix. Match the response ID exactly
        # before that suffix or the log punctuation, never an unrelated abort.
        return re.search(r"Aborted request\(s\) " + re.escape(request_id) + r"(?:-|[.,\s]|$)", text) is not None

    def verify(self, request_id=None):
        if self.before is None:
            return "client closed; backend cancellation UNVERIFIED (no evidence URL)"
        if self.log_path and not request_id:
            raise ValueError("the stream response ID is required for backend log evidence")
        deadline = time.monotonic() + positive_int("OLR_CANCEL_TIMEOUT_S", 30)
        while True:
            aborted, inflight = self.snapshot()
            if aborted < self.before:
                raise RuntimeError("backend counters reset during cancellation check")
            if aborted > self.before and inflight == 0:
                return "backend abort counter increased and no requests remain in flight"
            if self.log_path and self.logged_abort(request_id) and inflight == 0:
                return "backend abort log matched this response ID and no requests remain in flight"
            if time.monotonic() >= deadline:
                raise RuntimeError("backend did not confirm an abort with zero in-flight requests")
            time.sleep(0.25)
