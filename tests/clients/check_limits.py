# /// script
# requires-python = ">=3.10"
# dependencies = ["openai==3.49.0", "anthropic==2.8.0"]
# ///
"""Verify official SDK RateLimitError handling against a fresh limited user."""

import json
import os
from datetime import datetime
from urllib.request import Request, urlopen

import anthropic
import openai

base = os.environ.get("OLR_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
key = os.environ["OLR_API_KEY"]
model = os.environ.get("OLR_MODEL", "chat-model")


def snapshot():
    request = Request(base + "/v1/me/usage", headers={"Authorization": "Bearer " + key})
    with urlopen(request, timeout=10) as response:
        return json.load(response)


def expect_limit(call, error_class, protocol):
    try:
        call()
    except error_class as error:
        assert error.status_code == 429
        assert 1 <= int(error.response.headers["retry-after"]) <= 60
        body = error.response.json()
        assert body["error"]["type"] == "rate_limit_error"
        assert body["error"]["limit"] == "rpm"
        datetime.fromisoformat(body["error"]["reset_at"].replace("Z", "+00:00"))
        if protocol == "anthropic":
            assert body["type"] == "error"
    else:
        raise AssertionError(protocol + " did not raise RateLimitError")


before = snapshot()
remaining = before["remaining"]["rpm"]
assert isinstance(remaining, int) and 1 <= remaining <= 20, "use a fresh user with RPM <= 20"
with openai.OpenAI(base_url=base + "/v1", api_key=key, max_retries=0, timeout=10) as client:
    def chat():
        return client.chat.completions.create(
            model=model, max_tokens=1, messages=[{"role": "user", "content": "ping"}]
        )

    for _ in range(remaining):
        chat()
    expect_limit(chat, openai.RateLimitError, "openai")

with anthropic.Anthropic(base_url=base, api_key=key, max_retries=0, timeout=10) as client:
    expect_limit(
        lambda: client.messages.create(
            model=model, max_tokens=1, messages=[{"role": "user", "content": "ping"}]
        ),
        anthropic.RateLimitError,
        "anthropic",
    )

after = snapshot()
assert after["remaining"]["rpm"] == 0 and after["in_flight"] == 0
print("OpenAI and Anthropic SDK RateLimitError checks passed")
