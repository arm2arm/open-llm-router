# /// script
# requires-python = ">=3.10"
# dependencies = ["openai==3.49.0"]
# ///
"""Native stateless Responses checks for Open WebUI-compatible connections.

Run with uv run tests/clients/check_responses.py. OLR_BASE_URL is the server
root (without /v1); OLR_API_KEY and OLR_MODEL select the credential and model.
The backend must implement Responses; these checks never translate to Chat
Completions. Extended tests share the cancellation evidence settings described
in docs/CLIENTS.md.
"""

import json
import os
import sys

from openai import OpenAI
from client_checks import CancellationProbe, LONG_PROMPT, long_limits, mock_headers

BASE_URL = os.environ.get("OLR_BASE_URL", "http://127.0.0.1:8080").rstrip("/")
MODEL = os.environ.get("OLR_MODEL", "chat-model")
client = OpenAI(base_url=BASE_URL + "/v1", api_key=os.environ.get("OLR_API_KEY", "olr-not-checked"), max_retries=0, timeout=120)
QUESTION = [{"role": "user", "content": [{"type": "input_text", "text": "Describe the weather in Potsdam."}]}]
TOOL = {"type": "function", "name": "get_weather", "description": "Get the weather", "parameters": {"type": "object", "properties": {}, "additionalProperties": False}, "strict": True}


def text():
    r = client.responses.create(model=MODEL, input="Say hello.", instructions="Be concise.", max_output_tokens=128, store=False)
    assert r.output_text, "empty output_text"
    assert r.status in ("completed", "incomplete"), r.status
    assert r.usage.input_tokens > 0 and r.usage.output_tokens > 0, r.usage
    assert r.usage.total_tokens == r.usage.input_tokens + r.usage.output_tokens
    return f"{r.usage.input_tokens} + {r.usage.output_tokens} tokens"


def streaming():
    pieces, terminal = [], None
    with client.responses.create(model=MODEL, input=QUESTION, max_output_tokens=128, store=False, stream=True) as stream:
        for event in stream:
            if event.type == "response.output_text.delta":
                pieces.append(event.delta)
            if event.type in ("response.completed", "response.incomplete"):
                terminal = event.response
    assert len(pieces) >= 2, f"expected incremental text, got {len(pieces)} chunks"
    assert terminal and terminal.output_text == "".join(pieces), "lost terminal response or text"
    assert terminal.usage.input_tokens > 0 and terminal.usage.output_tokens > 0, terminal.usage
    return f"{len(pieces)} deltas, {terminal.usage.output_tokens} output tokens"


def tool_loop(streamed=False):
    params = dict(model=MODEL, input=QUESTION, max_output_tokens=128, store=False, tools=[TOOL], tool_choice={"type": "function", "name": "get_weather"})
    if streamed:
        with client.responses.stream(**params) as stream:
            list(stream)
            r = stream.get_final_response()
    else:
        r = client.responses.create(**params)
    calls = [item for item in r.output if item.type == "function_call"]
    assert len(calls) == 1 and calls[0].name == "get_weather", r.output
    call = calls[0]
    assert isinstance(json.loads(call.arguments), dict), call.arguments
    # This is Open WebUI's stateless tool loop: resend complete output items
    # and the matching tool result, with no provider-side response identifier.
    history = QUESTION + [item.model_dump(exclude_none=True) for item in r.output] + [{"type": "function_call_output", "call_id": call.call_id, "output": "12 degrees, light rain"}]
    followup = client.responses.create(model=MODEL, input=history, tools=[TOOL], tool_choice="none", max_output_tokens=128, store=False)
    assert followup.output_text, "no text after the tool result"
    return f"{call.name} -> tool result -> assistant text"


def streaming_tool_loop():
    return tool_loop(streamed=True)


def structured_output():
    r = client.responses.create(model=MODEL, input="Return an object with an answer string.", max_output_tokens=128, store=False,
                                text={"format": {"type": "json_schema", "name": "answer", "strict": True, "schema": {"type": "object", "properties": {"answer": {"type": "string"}}, "required": ["answer"], "additionalProperties": False}}})
    assert isinstance(json.loads(r.output_text)["answer"], str), r.output_text
    return "JSON schema output"


def long_stream():
    if os.environ.get("OLR_CHECK_EXTENDED") != "1":
        return None
    minimum, maximum = long_limits()
    with client.responses.stream(model=MODEL, input=LONG_PROMPT, max_output_tokens=maximum, store=False, extra_headers=mock_headers(maximum)) as stream:
        events = list(stream)
        r = stream.get_final_response()
    assert sum(event.type == "response.output_text.delta" for event in events) >= 2
    assert r.usage.output_tokens >= minimum, r.usage
    return f"{r.usage.output_tokens} output tokens"


def cancel_stream():
    if os.environ.get("OLR_CHECK_EXTENDED") != "1":
        return None
    probe, pieces, response_id = CancellationProbe(), 0, None
    with client.responses.create(model=MODEL, input=LONG_PROMPT, max_output_tokens=4096, store=False, stream=True, extra_headers=mock_headers(4096)) as stream:
        for event in stream:
            if event.type == "response.created":
                response_id = event.response.id
            if event.type == "response.output_text.delta":
                pieces += 1
                if pieces >= 3:
                    break
        else:
            raise AssertionError("stream ended before cancellation")
    return probe.verify(response_id)


def main():
    failed = 0
    print(f"OpenAI Responses SDK against {BASE_URL} (model {MODEL})")
    for check in [text, streaming, tool_loop, streaming_tool_loop, structured_output, long_stream, cancel_stream]:
        try:
            detail = check()
            print(f"  skip  {check.__name__}" if detail is None else f"  ok    {check.__name__}: {detail}")
        except Exception as error:
            failed += 1
            print(f"  FAIL  {check.__name__}: {type(error).__name__}: {error}")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
