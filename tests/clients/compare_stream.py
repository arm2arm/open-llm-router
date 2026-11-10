# /// script
# requires-python = ">=3.10"
# dependencies = ["openai==3.49.0"]
# ///
"""Compare one long OpenAI generation directly and through the router; see docs/CLIENTS.md."""

import json
import os
import sys
import time

from openai import OpenAI
from client_checks import LONG_PROMPT, comparison, long_limits, mock_headers, positive_int


def measure(client, model):
    minimum, maximum = long_limits()
    started = time.perf_counter()
    first, last, usage, finish = None, None, None, None
    with client.chat.completions.create(
        model=model, messages=[{"role": "user", "content": LONG_PROMPT}],
        max_tokens=maximum, temperature=0, stream=True, stream_options={"include_usage": True},
        extra_headers=mock_headers(maximum),
        **({"reasoning_effort": "none"} if os.environ.get("OLR_DISABLE_THINKING") == "1" else {}),
    ) as stream:
        for chunk in stream:
            usage = chunk.usage or usage
            for choice in chunk.choices:
                delta = choice.delta
                if delta.content or getattr(delta, "reasoning_content", None) or getattr(delta, "reasoning", None):
                    last = time.perf_counter()
                    if first is None:
                        first = last
                finish = choice.finish_reason or finish
    if finish not in ("stop", "length") or not usage or usage.completion_tokens < minimum:
        raise ValueError("generation incomplete, missing usage, or shorter than OLR_LONG_MIN_TOKENS")
    if first is None or last <= first:
        raise ValueError("generation did not arrive incrementally")
    return {
        "ttft_ms": (first - started) * 1000,
        "tokens_per_second": (usage.completion_tokens - 1) / (last - first),
        "output_tokens": usage.completion_tokens,
    }


def main():
    # Require an explicit direct endpoint: never silently compare the router to itself.
    direct_url = os.environ["OLR_DIRECT_BASE_URL"].rstrip("/")
    router_url = os.environ.get("OLR_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
    if direct_url == router_url:
        raise ValueError("direct and router URLs must differ")
    model = os.environ.get("OLR_MODEL", "chat-model")
    direct_model = os.environ.get("OLR_UPSTREAM_MODEL", model)
    samples = {"direct": [], "router": []}
    with OpenAI(base_url=direct_url + "/v1", api_key=os.environ.get("OLR_DIRECT_API_KEY") or "olr-not-checked",
                max_retries=0, timeout=120) as direct, OpenAI(
        base_url=router_url + "/v1", api_key=os.environ.get("OLR_API_KEY") or "olr-not-checked",
        max_retries=0, timeout=120,
    ) as router:
        routes = [("direct", direct, direct_model), ("router", router, model)]
        # Warm up both paths, then alternate order to reduce prefix-cache/order bias.
        for _, client, name in routes:
            measure(client, name)
        for index in range(positive_int("OLR_BENCH_RUNS", 3)):
            for route, client, name in routes[::1 if index % 2 == 0 else -1]:
                sample = measure(client, name)
                samples[route].append(sample)
                print(json.dumps({"route": route, "run": index + 1, **sample}), flush=True)
    result = comparison(samples)
    print(json.dumps(result, indent=2))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
