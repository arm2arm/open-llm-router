# /// script
# requires-python = ">=3.10"
# dependencies = ["anthropic==2.8.0"]
# ///
"""An exhausted calendar quota must not make the default Anthropic SDK sleep.

Use an account with an exhausted daily quota and available RPM/concurrency.
The parent deadline catches a regression even when Retry-After is many minutes.
"""

import os
import subprocess
import sys
import time

import anthropic
import httpx2


def check():
    requests = []
    started = time.monotonic()
    # max_retries is deliberately omitted: exercise the SDK's default policy.
    with httpx2.Client(event_hooks={"request": [requests.append]}) as transport:
        with anthropic.Anthropic(
            base_url=os.environ["OLR_BASE_URL"],
            api_key=os.environ["OLR_API_KEY"],
            timeout=3,
            http_client=transport,
        ) as client:
            try:
                client.messages.create(
                    model=os.environ.get("OLR_MODEL", "chat-model"),
                    max_tokens=1,
                    messages=[{"role": "user", "content": "ping"}],
                )
            except anthropic.RateLimitError as error:
                assert error.status_code == 429
                assert error.response.headers["x-should-retry"] == "false"
                assert int(error.response.headers["retry-after"]) >= 1
                assert error.response.json()["error"]["limit"] == "tokens_per_day"
                assert len(requests) == 1, "default SDK retried an exhausted daily quota"
                assert time.monotonic() - started < 3, "calendar quota response was delayed"
                print("ok: default Anthropic SDK returns exhausted daily quota without retry")
            else:
                raise AssertionError("expected exhausted daily quota")


if __name__ == "__main__":
    if sys.argv[1:] == ["--request"]:
        check()
    else:
        try:
            subprocess.run([sys.executable, __file__, "--request"], check=True, timeout=5)
        except subprocess.TimeoutExpired:
            raise SystemExit("FAIL: default Anthropic SDK slept/retried after daily quota exhaustion")
