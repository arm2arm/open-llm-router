# Client compatibility checks

Issue [#8](https://github.com/arm2arm/open-llm-router/issues/8) checks real clients against a real
backend before the pilot. The router forwards each backend's native protocols; it does not
implement missing backend APIs. Endpoint URLs and credentials belong in the environment.

## Recorded run: 2026-11-11

`make sdk-limits` runs a separate fresh limited user through the router and verifies
that both official SDKs raise `RateLimitError` for HTTP 429, with `Retry-After` and the
structured limit/reset fields. This check runs in CI after the successful-request SDK suite.

Tested on the laptop with Ollama **0.40.16**, a local Qwen 27B MLX model, OpenAI Python
**3.49.0**, Anthropic Python **2.8.0**, Claude Code **2.1.322**, and OpenCode **1.18.10**.
The router used one replica and the aliases in `config.real.example.yaml`.

| Check | Result |
|---|---|
| OpenAI text, SSE with/without usage, tools and tool-result round trip, model listing | Pass, direct and routed |
| Anthropic text, SSE events, tools and tool-result round trip | Pass, direct and routed |
| Long streaming output | Pass, both SDKs, direct and routed; at least 128 output tokens with a 512-token limit |
| Anthropic thinking blocks | Pass, direct and routed |
| Beta header and `cache_control` fields | Accepted, direct and routed; this does not establish cache hits |
| Claude Code tool session | Pass: five turns; read files, edit the tax calculation, run `python3 -m unittest -v`; independently rerun test passed |
| OpenCode tool session | Pass: four model steps; two reads, edit, shell test, final answer; independently rerun test passed |
| curl tool round trip | Pass: forced `get_weather` call, then text after returning the tool result |
| OpenAI legacy `/v1/completions` | Empty text from this model with the check's raw prompt, direct and routed; check remains a failure |
| Anthropic `/v1/messages/count_tokens` | Backend returns 404, direct and routed; check remains a failure |
| Embeddings | Not checked: no embedding model selected |
| Cancel after three streamed text chunks | Both SDKs close successfully; Ollama backend abort evidence is **not verified** |
| Mock regressions | `make sdk` passes both protocols through the router and each mock, including backend-confirmed cancellation |

Timing observations (three paired samples per path after warming up):

| Trial | Direct median TTFT / tokens/s | Router median TTFT / tokens/s | Gate |
|---|---|---|---|
| Initial numbered-list prompt, 145–147 output tokens | 79.97 ms / 50.68 | 80.39 ms / 46.88 | Fail: 7.5% throughput loss |
| Final essay prompt, 512 output tokens every run, no overlapping test work | 72.99 ms / 54.05 | 78.67 ms / 58.62 | Fail: 5.68 ms added median TTFT |

The final long-generation trial had no throughput loss, but did not meet the strict TTFT
gate. The two trials failed different thresholds; a performance pass is **not established**.
These small samples cannot isolate backend variation from router overhead. Repeat on the
deployed GPU backend and investigate under #18 before claiming the performance budget is met.

`make sdk-real` deliberately exits nonzero for the completions and count-tokens failures above.
Do not turn those failures into skips to claim full backend compatibility.

The Ollama run is a useful compatibility check, but the vLLM/SGLang go/no-go remains **pending**.
Repeat on the deployed backend, verify generation stops there, and record the decision in
[issue #1](https://github.com/arm2arm/open-llm-router/issues/1). No vLLM or SGLang endpoint was
available for this run. The 256-stream performance acceptance test belongs to #18.

## Connect a real backend

Build the host binary and export these variables in both the router and client shells:

```sh
make build
docker compose up -d postgres
export OLR_DATABASE_URL='postgres://olr:olr-dev-only@127.0.0.1:5432/olr?sslmode=disable'
export OLR_DIRECT_BASE_URL=http://127.0.0.1:11434  # origin only, without /v1
export OLR_UPSTREAM_MODEL=your-served-model
export OLR_DIRECT_API_KEY=ollama                  # nonempty placeholder without backend auth
export OLR_HEALTH_PATH=/                         # /health for vLLM and SGLang
export OLR_BASE_URL=http://127.0.0.1:18080
export OLR_MODEL=chat-model
user_id=$(bin/olr admin user add -username real-backend-test -groups staff)
export OLR_API_KEY=$(bin/olr admin key create -user "$user_id" -label real-backend-test)
bin/olr serve -listen 127.0.0.1:18080 -config config.real.example.yaml
```

For vLLM or SGLang, change the origin/model/key and use `OLR_HEALTH_PATH=/health` if that
backend exposes it. `health_path` defaults to `/health` on ordinary pool replicas. Ollama
returns 404 there, so it needs `/`. Backend authentication uses `bearer_token_env`; client
credentials are replaced by the backend credential. The router requires its own API key on
every `/v1/*` route. Keep `OLR_API_KEY` separate from the backend's `OLR_DIRECT_API_KEY`.
For an existing bootstrap user, reuse its UUID instead of running `user add` again.

The example advertises both protocols. If a deployed backend lacks `/v1/messages`, configure
`protocols: [openai]` in a private copy and use the OpenAI checks alone; it is not an Anthropic pass.
vLLM must have a tool-capable model and the matching tool parser; see its
[Claude Code integration guide](https://docs.vllm.ai/en/latest/serving/integrations/claude_code/).
SGLang protocol support must be checked on the actual installed version.

## SDK runs and cancellation evidence

```sh
# Ollama's local reasoning model otherwise spends short text-test budgets on thinking.
export OLR_DISABLE_THINKING=1
export OLR_CHECK_THINKING=1   # only if the selected model emits thinking blocks
make sdk-real               # direct AND routed, reporting all checks even after a failure
```

`OLR_DISABLE_THINKING=1` sends OpenAI `reasoning_effort=none` and Anthropic
`thinking.type=disabled`, except in the explicit thinking check. Other engines may require
different model-specific controls; leave it unset unless supported. The Ollama
[OpenAI reference](https://docs.ollama.com/api/openai-compatibility) describes reasoning controls;
its [Anthropic reference](https://docs.ollama.com/api/anthropic-compatibility) documents missing
count-tokens support and partial support for caching, tool choice and thinking budgets.
The local tool tests passed, but do not assume every tool-choice mode is enforced by Ollama.

The individual `uv run tests/clients/check_openai.py` and `check_anthropic.py` commands work
against `OLR_BASE_URL`; `OLR_CHECK_EXTENDED=1` enables long output, cancellation and Anthropic
beta/cache fields. `OLR_EMBEDDING_MODEL` enables embeddings. Long output defaults to a maximum
of 512 tokens and a required minimum of 128; override `OLR_LONG_MAX_TOKENS` and
`OLR_LONG_MIN_TOKENS` for the model. `OLR_CHECK_THINKING=1` requires a thinking block followed
by text with a 2048-token budget.

For a vLLM acceptance run, use one **idle replica**, with no other clients generating or
cancelling requests, and set:

```sh
export OLR_VLLM_METRICS_URL="$OLR_DIRECT_BASE_URL/metrics"
export OLR_REQUIRE_CANCEL_EVIDENCE=1
make sdk-real
```

The cancellation probe reads `vllm:request_success_total{finished_reason="abort"}` for
`OLR_UPSTREAM_MODEL` before closing the stream, then requires an increased abort counter and
zero running/waiting requests. It sums workers, checks that the backend was initially idle,
rejects missing metrics and counter resets, and waits up to `OLR_CANCEL_TIMEOUT_S` (default 30).
The metric names follow the [vLLM metrics reference](https://docs.vllm.ai/en/v0.31.0/design/metrics/).
Concurrent traffic makes aggregate metrics ambiguous. If the backend uses different metrics
(including SGLang), inspect its abort logs/counters and record the evidence manually.

Without an evidence URL, the check prints **backend cancellation UNVERIFIED**. With
`OLR_REQUIRE_CANCEL_EVIDENCE=1`, missing evidence fails. An early client close alone cannot
prove GPU generation stopped. Mock checks use `OLR_CANCEL_STATS_URLS`, a comma-separated list
of all replicas' `/_mock/stats` URLs, and require an abort plus zero in-flight requests.
`OLR_TEST_MOCK=1` enables per-request mock headers for the extended tests; leave it unset on
real backends. `make sdk` sets these automatically and exercises both replicas.

## Claude Code

Start in a disposable directory containing a fresh copy of `tests/clients/agent_fixture/`.
The fixture intentionally has a failing tax calculation; never run the session in the checked-in
fixture itself. Set the router root URL, credential and all model defaults:

```sh
export ANTHROPIC_BASE_URL="$OLR_BASE_URL"         # no /v1
export ANTHROPIC_API_KEY="$OLR_API_KEY"
export ANTHROPIC_AUTH_TOKEN="$OLR_API_KEY"
export ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-5
export ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-4-5
export ANTHROPIC_DEFAULT_OPUS_MODEL=claude-opus-4-1
export MAX_THINKING_TOKENS=0
export CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING=1
claude --bare --print --model claude-sonnet-4-5 --max-turns 8 \
  --no-session-persistence --allowedTools 'Read' 'Edit' 'Bash(python3 *)' \
  --output-format json \
  'Read invoice.py and test_invoice.py using tools. Fix total_with_tax so it adds tax as a fraction of the subtotal. Run python3 -m unittest -v using the Bash tool and report the result. Use only the files in this directory.'
python3 -m unittest -v
```

This is the tested Claude Code 2.1.322 invocation. `--bare` avoids personal hooks and settings;
its authentication uses `ANTHROPIC_API_KEY`. The router aliases resolve all three defaults to
`chat-model`, which is rewritten to `OLR_UPSTREAM_MODEL`. There is no need to copy backend
models to Claude-style names. Disable reasoning only for the initial compatibility pass;
test thinking separately. As the
[vLLM integration guide](https://docs.vllm.ai/en/latest/serving/integrations/claude_code/) explains,
vLLM does not implement Anthropic's hosted tools such as WebSearch;
local read/edit/shell tools exercise the required client tool loop.

## OpenCode

For the tested **1.18.10** release, put this `opencode.json` in a separate fresh fixture copy.
The local URL is an example; replace it with the router origin plus `/v1`. Keep credentials
in `OLR_API_KEY`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "olr/chat-model",
  "small_model": "olr/chat-model",
  "provider": {
    "olr": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Local router",
      "options": {"baseURL": "http://127.0.0.1:18080/v1", "apiKey": "{env:OLR_API_KEY}"},
      "models": {
        "chat-model": {
          "name": "Local model",
          "limit": {"context": 32768, "output": 4096},
          "options": {"reasoningEffort": "none"}
        }
      }
    }
  }
}
```

Run `opencode run --pure --model olr/chat-model --format json` with the same prompt as Claude
Code, then independently run `python3 -m unittest -v`. Confirm completed read, edit and bash
tool events and a successful test, rather than relying on the model's final claim.
Use a compatible provider that sends `/v1/chat/completions`; this router does not serve
`/v1/responses`. Newer OpenCode releases use a different `providers`/`package`/`settings`
schema; consult that release's [provider documentation](https://opencode.ai/docs/providers/).

## curl tool round trip

This uses curl and jq against the same router and performs two model requests with a tool
result between them. Use a disposable directory for the response files:

```sh
jq -n --arg model "$OLR_MODEL" '{model:$model,max_tokens:128,reasoning_effort:"none",
  messages:[{role:"user",content:"Use get_weather to check Potsdam weather."}],
  tools:[{type:"function",function:{name:"get_weather",description:"Get weather for a city",
    parameters:{type:"object",properties:{city:{type:"string"}},required:["city"]}}}],
  tool_choice:{type:"function",function:{name:"get_weather"}}}' > first.json
curl --fail --silent --show-error "$OLR_BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $OLR_API_KEY" -H 'Content-Type: application/json' \
  --data-binary @first.json > call.json
jq -e '.choices[0].message.tool_calls[0].function.name == "get_weather"' call.json
jq --slurpfile response call.json '.messages += [$response[0].choices[0].message,
  {role:"tool",tool_call_id:$response[0].choices[0].message.tool_calls[0].id,
   content:"12 degrees, light rain"}] | .tool_choice = "none"' first.json > second.json
curl --fail --silent --show-error "$OLR_BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $OLR_API_KEY" -H 'Content-Type: application/json' \
  --data-binary @second.json | jq -e '.choices[0].message.content | length > 0'
```

The `reasoning_effort` setting above is for the tested Ollama model; remove it when unsupported.

## Streaming timing comparison

With an idle single replica and both origins set as above:

```sh
make bench-real
# More paired samples, if needed:
OLR_BENCH_RUNS=5 make bench-real
```

This warms up both connections, alternates direct/routed request order, and prints per-run
output tokens, time to first content/reasoning delta, and decode tokens/s from backend usage.
Decode rate is `(output_tokens - 1) / (last_delta_time - first_delta_time)`. Final usage-only
chunks are excluded from the timing. Default runs use three samples per path and the same
long prompt, token limit, and temperature zero. A short/truncated stream without a finish
reason or usage fails. Reports contain timing metadata, not generated content or credentials.

The command fails if median added TTFT is 5 ms or more or median throughput loss exceeds 2%.
This is a single-stream diagnostic, not a p95/load-test result. Cache state, output length,
thermal throttling and other traffic affect results; investigate failed comparisons on an
idle backend before attributing a difference to the router. Record both medians and sample
counts with the final go/no-go.
