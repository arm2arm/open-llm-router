# /// script
# requires-python = ">=3.10"
# dependencies = ["anthropic==2.8.0"]
# ///
"""Checks an Anthropic-compatible endpoint with the official anthropic SDK.

    uv run tests/clients/check_anthropic.py

Environment:
    OLR_BASE_URL        server root, without /v1 (default http://127.0.0.1:8080)
    OLR_API_KEY         API key (default: a placeholder; the mock ignores it)
    OLR_MODEL           model (default chat-model)
    OLR_CHECK_THINKING  set to 1 to also ask for a thinking block

The checks hold for any correct backend, mock or real: they look at the shape
of the answers, not at their text. Exit status 1 if a check fails.
"""

import os
import sys

from anthropic import Anthropic

BASE_URL = os.environ.get("OLR_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
MODEL = os.environ.get("OLR_MODEL", "chat-model")

client = Anthropic(
    base_url=BASE_URL,
    api_key=os.environ.get("OLR_API_KEY", "olr-not-checked"),
    max_retries=0,
    timeout=120,
)

QUESTION = [{"role": "user", "content": "Describe the weather in Potsdam in one sentence."}]
WEATHER_TOOL = {
    "name": "get_weather",
    "description": "Get the current weather for a city.",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
    },
}
CALL_WEATHER = {"type": "tool", "name": "get_weather"}


def blocks(message, kind):
    return [b for b in message.content if b.type == kind]


def text():
    m = client.messages.create(model=MODEL, max_tokens=32, messages=QUESTION)
    assert m.role == "assistant" and m.type == "message"
    assert blocks(m, "text") and blocks(m, "text")[0].text, "no text"
    assert m.stop_reason in ("end_turn", "max_tokens"), m.stop_reason
    assert m.usage.input_tokens > 0 and m.usage.output_tokens > 0, m.usage
    return f"{m.usage.input_tokens} + {m.usage.output_tokens} tokens"


def stream():
    pieces = []
    with client.messages.stream(model=MODEL, max_tokens=32, messages=QUESTION) as s:
        for piece in s.text_stream:
            pieces.append(piece)
        final = s.get_final_message()
    assert len(pieces) >= 2, f"expected several text chunks, got {len(pieces)}"
    assert "".join(pieces) == blocks(final, "text")[0].text
    assert final.usage.input_tokens > 0 and final.usage.output_tokens > 0, final.usage
    return f"{len(pieces)} chunks, {final.usage.output_tokens} output tokens"


def stream_events():
    events = [e.type for e in client.messages.create(model=MODEL, max_tokens=8, messages=QUESTION, stream=True)]
    assert events[0] == "message_start", events
    assert events[-1] == "message_stop", events
    for expected in ("content_block_start", "content_block_delta", "content_block_stop", "message_delta"):
        assert expected in events, f"no {expected} in {events}"
    return f"{len(events)} events"


def tool_use():
    m = client.messages.create(
        model=MODEL, max_tokens=128, messages=QUESTION, tools=[WEATHER_TOOL], tool_choice=CALL_WEATHER,
    )
    calls = blocks(m, "tool_use")
    assert calls, "no tool_use block"
    assert calls[0].name == "get_weather", calls[0].name
    assert isinstance(calls[0].input, dict)
    assert m.stop_reason == "tool_use", m.stop_reason

    # Second round: hand the tool's result back and get text.
    m2 = client.messages.create(
        model=MODEL, max_tokens=32, tools=[WEATHER_TOOL], tool_choice={"type": "none"},
        messages=QUESTION + [
            {"role": "assistant", "content": [b.model_dump(exclude_none=True) for b in m.content]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": calls[0].id, "content": "12 degrees, light rain"},
            ]},
        ],
    )
    assert blocks(m2, "text"), "no text after the tool result"
    return f"{calls[0].name}({calls[0].input})"


def tool_use_stream():
    with client.messages.stream(
        model=MODEL, max_tokens=128, messages=QUESTION, tools=[WEATHER_TOOL], tool_choice=CALL_WEATHER,
    ) as s:
        final = s.get_final_message()
    calls = blocks(final, "tool_use")
    assert calls and calls[0].name == "get_weather", final.content
    assert isinstance(calls[0].input, dict), calls[0].input
    return f"{calls[0].name}({calls[0].input})"


def count_tokens():
    r = client.messages.count_tokens(model=MODEL, messages=QUESTION)
    assert r.input_tokens > 0, r
    return f"{r.input_tokens} input tokens"


def thinking():
    if not os.environ.get("OLR_CHECK_THINKING"):
        return None
    with client.messages.stream(
        model=MODEL, max_tokens=2048, messages=QUESTION, thinking={"type": "enabled", "budget_tokens": 1024},
    ) as s:
        final = s.get_final_message()
    assert blocks(final, "thinking") and blocks(final, "thinking")[0].thinking, final.content
    assert blocks(final, "text"), "no text after the thinking block"
    return "thinking block, then text"


def main():
    checks = [text, stream, stream_events, tool_use, tool_use_stream, count_tokens, thinking]
    failed = 0
    print(f"anthropic SDK against {BASE_URL} (model {MODEL})")
    for check in checks:
        name = check.__name__.replace("_", " ")
        try:
            detail = check()
        except Exception as e:  # report every check, then fail
            failed += 1
            print(f"  FAIL  {name}: {type(e).__name__}: {e}")
            continue
        print(f"  skip  {name}" if detail is None else f"  ok    {name}: {detail}")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
