# /// script
# requires-python = ">=3.10"
# dependencies = ["openai==3.49.0"]
# ///
"""Checks an OpenAI-compatible endpoint with the official openai SDK.

    uv run tests/clients/check_openai.py

Environment:
    OLR_BASE_URL          server root, without /v1 (default http://127.0.0.1:8080)
    OLR_API_KEY           API key (default: a placeholder; the mock ignores it)
    OLR_MODEL             chat model (default chat-model)
    OLR_EMBEDDING_MODEL   embedding model; the embeddings check is skipped without it

The checks hold for any correct backend, mock or real: they look at the shape
of the answers, not at their text. Exit status 1 if a check fails.
"""

import json
import os
import sys

from openai import OpenAI

BASE_URL = os.environ.get("OLR_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
MODEL = os.environ.get("OLR_MODEL", "chat-model")
EMBEDDING_MODEL = os.environ.get("OLR_EMBEDDING_MODEL")

client = OpenAI(
    base_url=BASE_URL + "/v1",
    api_key=os.environ.get("OLR_API_KEY", "olr-not-checked"),
    max_retries=0,
    timeout=120,
)

QUESTION = [{"role": "user", "content": "Describe the weather in Potsdam in one sentence."}]
WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather for a city.",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"],
        },
    },
}
CALL_WEATHER = {"type": "function", "function": {"name": "get_weather"}}


def chat_text():
    r = client.chat.completions.create(model=MODEL, messages=QUESTION, max_tokens=32)
    choice = r.choices[0]
    assert choice.message.role == "assistant"
    assert choice.message.content, "empty content"
    assert choice.finish_reason in ("stop", "length"), choice.finish_reason
    assert r.usage.prompt_tokens > 0 and r.usage.completion_tokens > 0, r.usage
    assert r.usage.total_tokens == r.usage.prompt_tokens + r.usage.completion_tokens
    return f"{r.usage.prompt_tokens} + {r.usage.completion_tokens} tokens"


def chat_stream():
    stream = client.chat.completions.create(
        model=MODEL, messages=QUESTION, max_tokens=32, stream=True,
        stream_options={"include_usage": True},
    )
    pieces, finish, usage = [], None, None
    for chunk in stream:
        if chunk.usage is not None:
            usage = chunk.usage
        for choice in chunk.choices:
            if choice.delta.content:
                pieces.append(choice.delta.content)
            finish = choice.finish_reason or finish
    assert len(pieces) >= 2, f"expected several content chunks, got {len(pieces)}"
    assert finish in ("stop", "length"), finish
    assert usage is not None and usage.completion_tokens > 0, "no usage chunk"
    return f"{len(pieces)} chunks, {usage.completion_tokens} output tokens"


def chat_stream_without_usage():
    stream = client.chat.completions.create(model=MODEL, messages=QUESTION, max_tokens=8, stream=True)
    chunks = list(stream)
    assert chunks, "no chunks"
    assert all(chunk.choices for chunk in chunks), "a chunk without choices although usage was not asked for"
    return f"{len(chunks)} chunks"


def tool_call():
    r = client.chat.completions.create(
        model=MODEL, messages=QUESTION, max_tokens=128, tools=[WEATHER_TOOL], tool_choice=CALL_WEATHER,
    )
    message = r.choices[0].message
    assert message.tool_calls, "no tool call"
    call = message.tool_calls[0]
    assert call.function.name == "get_weather", call.function.name
    assert isinstance(json.loads(call.function.arguments), dict)

    # Second round: hand the tool's result back and get text.
    r = client.chat.completions.create(
        model=MODEL, max_tokens=32, tools=[WEATHER_TOOL], tool_choice="none",
        messages=QUESTION + [
            message.model_dump(exclude_none=True),
            {"role": "tool", "tool_call_id": call.id, "content": "12 degrees, light rain"},
        ],
    )
    assert r.choices[0].message.content, "no text after the tool result"
    return f"{call.function.name}({call.function.arguments})"


def tool_call_stream():
    stream = client.chat.completions.create(
        model=MODEL, messages=QUESTION, max_tokens=128, tools=[WEATHER_TOOL], tool_choice=CALL_WEATHER,
        stream=True,
    )
    name, arguments, finish = "", "", None
    for chunk in stream:
        for choice in chunk.choices:
            for call in choice.delta.tool_calls or []:
                name += call.function.name or ""
                arguments += call.function.arguments or ""
            finish = choice.finish_reason or finish
    assert name == "get_weather", name
    assert isinstance(json.loads(arguments), dict), arguments
    assert finish in ("tool_calls", "stop"), finish
    return f"{name}({arguments})"


def completion():
    r = client.completions.create(model=MODEL, prompt="Once upon a time", max_tokens=8)
    assert r.choices[0].text, "empty text"
    assert r.usage.completion_tokens > 0, r.usage
    return f"{r.usage.completion_tokens} output tokens"


def embeddings():
    if not EMBEDDING_MODEL:
        return None
    r = client.embeddings.create(model=EMBEDDING_MODEL, input=["first text", "second text"])
    assert len(r.data) == 2, len(r.data)
    assert len(r.data[0].embedding) > 0 and isinstance(r.data[0].embedding[0], float)
    assert r.data[0].embedding != r.data[1].embedding, "two inputs gave the same vector"
    return f"2 vectors of {len(r.data[0].embedding)}"


def models():
    ids = [m.id for m in client.models.list()]
    assert MODEL in ids, f"{MODEL} is not in {ids}"
    return ", ".join(ids)


def main():
    checks = [chat_text, chat_stream, chat_stream_without_usage, tool_call, tool_call_stream,
              completion, embeddings, models]
    failed = 0
    print(f"openai SDK against {BASE_URL} (model {MODEL})")
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
