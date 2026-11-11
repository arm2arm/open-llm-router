# Client compatibility checks

Issue [#8](https://github.com/arm2arm/open-llm-router/issues/8) checks real clients against a real
backend before the pilot. The router forwards each backend's native protocols; it does not
implement missing backend APIs. Endpoint URLs and credentials belong in the environment.

## Result for #8

**Go**, recorded in [issue #1](https://github.com/arm2arm/open-llm-router/issues/1). Both
official SDKs, Claude Code, OpenCode and curl work through the router against real vLLM 0.33.0,
closing a stream stops generation on the backend, and the router adds no noticeable latency.
Every failure below reproduces on a direct connection to the backend; no router-side problem
was found. Three things remain open and are not covered by this result:

- The deployed GPU backend was not reachable. Repeat `make sdk-real`, `make bench-real` and one
  session per client there before the pilot. All runs below are on one laptop.
- vLLM 0.33.0 does not count aborted requests in its metrics, so cancellation evidence needs
  the backend log as well as `/metrics` (see **Local vLLM on an ARM64 CPU**).
- The model decides whether coding clients work. Of the three models tried on vLLM, only
  Qwen3 8B completed the tool sessions; see the two vLLM runs.

## Recorded local vLLM Metal run: 2026-11-11

Tested **vLLM 0.33.0** with the **vllm-metal 0.33.0** plugin on an Apple M5 Max (macOS 26.6.2),
serving **mlx-community/Qwen3-8B-4bit** as `qwen3-8b`: 32,768-token context, 6 GiB KV cache,
`hermes` tool parser, `qwen3` reasoning parser. The backend and a host router were bound to
loopback ports 18000 and 18080, with an isolated test database. SDK and CLI versions match the
Ollama run below. The setup is under **Local vLLM Metal on Apple Silicon**.

| Check | Result |
|---|---|
| OpenAI SDK | All nine enabled checks pass, direct and routed: text, both SSE modes, tool calls and tool-result round trip, streamed tool calls, legacy completions, models, long output and cancellation |
| Anthropic SDK | Nine of ten enabled checks pass, direct and routed. The thinking check fails on both paths: vLLM Metal answers HTTP 400 to `thinking.budget_tokens` (`thinking_token_budget` is not supported) |
| Thinking without a budget | A request that leaves `thinking` unset returns a thinking block and then text, direct and routed (one curl request per path) |
| Long output | 512 output tokens on both protocols and both paths |
| Backend cancellation | All four cases confirmed by a fresh response-ID-matched vLLM abort log and zero running/waiting requests; the abort metric stayed zero |
| Claude Code | **Pass, 3 of 3 runs.** Five turns each: two reads, one edit, `python3 -m unittest -v`; test file unchanged; independently rerun test passed |
| OpenCode, thinking left on | **Pass, 3 of 3 runs.** Two runs took four model steps (two reads, edit, shell test, answer). One run had eight edits rejected for a non-matching `oldString` before a successful edit and did not rerun the test itself; the independent rerun passed |
| OpenCode, `reasoningEffort: none` | Fail, 0 of 4 runs: the model asked to read `/invoice.py`; OpenCode rejected the path outside the project and ended the session with exit code zero |
| curl tool round trip | Pass: forced `get_weather` call, tool-result request, final text |
| Embeddings | Not checked: no embedding model selected |

`make sdk-real` exits **nonzero**: 36 of the 38 enabled SDK checks pass, the two thinking
checks fail as described, and the two embeddings checks are skipped. Beta/cache acceptance
does not establish cache hits.

`make bench-real` passes for three paired 512-token generations after warmup, alternating
direct/router order with no other model traffic:

| Path | Median TTFT | Median decode tokens/s |
|---|---:|---:|
| Direct | 40.99 ms | 103.23 |
| Router | 41.50 ms | 103.12 |

The median difference is 0.51 ms TTFT and 0.11% throughput loss, within the diagnostic's
5 ms / 2% gates. This is a single-stream check; the 256-stream targets belong to #18.

The vLLM process tree measured 6.75 GiB with `footprint` after all tests, and the per-process
peaks sum to 7.49 GiB, including one 31,419-token request. The weights are 4.3 GiB and the KV
budget is 6 GiB, so allow about 12 GiB if the whole cache becomes resident. That is under the
16 GB target for this run, but the host has 128 GiB; a 16 GB machine was not tested.

**Qwen2.5-Coder-7B-Instruct-4bit** on the same server (`hermes` parser, 16,384-token context)
passed all 36 applicable SDK checks, the four cancellation cases and the timing comparison
(38.68 ms / 109.27 tokens/s direct, 39.60 ms / 109.11 routed). It failed both coding clients:
with automatic tool choice it writes the call as JSON in a code fence, not in `<tool_call>`
tags, so vLLM returns plain text and the client stops after one turn with exit code zero.
A direct request behaves the same, and naming the format in the prompt did not help. The SDK
tool checks pass because they force the tool choice.

## Recorded local vLLM CPU run: 2026-11-11

Tested genuine **vLLM 0.33.0**, **Qwen/Qwen3-0.6B** (served as `tiny-qwen`), FP32,
eight ARM CPU cores inside Docker on Apple Silicon. The host has 128 GiB RAM; Docker has
64 GiB available. The backend and a separate router were bound to loopback ports 18000 and
18080, with an isolated test database. SDK and CLI versions match the Ollama run below.
The pinned backend setup is documented under **Local vLLM on an ARM64 CPU**. This is the
only run in which every enabled SDK check passes, including thinking with a token budget.

| Check | Result |
|---|---|
| OpenAI SDK | All nine enabled checks pass, direct and routed: text, both SSE modes, tool calls and tool-result round trip, streamed tool calls, legacy completions, models, long output and cancellation |
| Anthropic SDK | All ten enabled checks pass, direct and routed: text, SSE/events, tool calls and tool-result round trip, streamed tools, count-tokens, thinking, beta/cache fields, long output and cancellation |
| Long output | 512 output tokens on both protocols and both paths |
| Backend cancellation | All four cases confirmed by a fresh response-ID-matched vLLM abort log and zero running/waiting requests; the abort metric alone stayed zero |
| curl tool round trip | Pass: forced weather tool, tool-result request, final text |
| Embeddings | Not checked: no embedding model selected |
| Claude Code | Transport and read/edit/shell tools work, but the fixture fails: the model renamed the function incorrectly; a second, explicitly guided attempt produced malformed Edit JSON and still failed the independently rerun test |
| OpenCode | Transport and read/write/shell tools work, but the fixture fails: the model overwrote the test with an invalid pytest import and left the calculation unchanged; restoring the original test and rerunning still fails |
| Mock regression and probe tests | `make sdk` and all 13 `make test-clients` tests pass |

`make sdk-real` exits **zero** with the documented log evidence enabled: 38 enabled SDK
checks pass, with the two embeddings checks explicitly skipped. Without the log evidence,
all four abort-counter checks fail; this reproduces the upstream metrics issue rather than
establishing a router cancellation failure. Beta/cache acceptance does not establish cache hits.

`make bench-real` passes for three paired 512-token generations after warmup, alternating
direct/router order with no other model traffic:

| Path | Median TTFT | Median decode tokens/s |
|---|---:|---:|
| Direct | 134.11 ms | 45.00 |
| Router | 130.26 ms | 44.81 |

The median difference is -3.84 ms TTFT and 0.41% throughput loss, within the diagnostic's
5 ms / 2% gates. This is a single-stream CPU check; it does not establish the p95,
256-concurrent-stream GPU performance targets in #18.

The protocol and backend-cancellation checks pass. The 0.6B model is too weak for the
coding-client task; the Metal run above covers that. Both CLI processes exited zero despite
their failed tasks, so a successful exit or a model's claim that tests passed is not evidence:
inspect the tool events and independently rerun the original fixture, checking that the test
file was not modified.

For this 16k-context model, Claude Code used `CLAUDE_CODE_MAX_OUTPUT_TOKENS=2048` and
`CLAUDE_CODE_MAX_CONTEXT_TOKENS=16384`, and OpenCode used context/output limits of 16384/4096.

## Recorded Ollama run: 2026-11-11

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

The Ollama run is a compatibility check only; the two vLLM runs above carry the go/no-go.
SGLang has not been tested. The 256-stream performance acceptance test belongs to #18.

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

### Local vLLM Metal on Apple Silicon

The [vllm-metal](https://github.com/vllm-project/vllm-metal) plugin runs vLLM on the Mac GPU.
The tested install is the stable channel of its
[install script](https://github.com/vllm-project/vllm-metal/blob/main/docs/installation.md),
which creates `~/.venv-vllm-metal`. The model download is about 4.6 GB; the revision is pinned.

```sh
source ~/.venv-vllm-metal/bin/activate
vllm serve mlx-community/Qwen3-8B-4bit \
  --revision 545dc4251c05440727734bcd94334791f6ab0192 \
  --served-model-name qwen3-8b --host 127.0.0.1 --port 18000 \
  --max-model-len 32768 --max-num-seqs 2 --max-num-batched-tokens 2048 \
  --kv-cache-memory-bytes 6442450944 \
  --enable-auto-tool-choice --tool-call-parser hermes --reasoning-parser qwen3 \
  --generation-config vllm --enable-log-requests --max-log-len 0 \
  > /tmp/olr-vllm-metal.log 2>&1 &

export OLR_DIRECT_BASE_URL=http://127.0.0.1:18000
export OLR_UPSTREAM_MODEL=qwen3-8b
export OLR_DIRECT_API_KEY=local-vllm-no-auth
export OLR_HEALTH_PATH=/health
export OLR_DISABLE_THINKING=1
export OLR_CHECK_THINKING=1   # expected to fail here: thinking budgets are unsupported
export OLR_VLLM_METRICS_URL="$OLR_DIRECT_BASE_URL/metrics"
export OLR_VLLM_LOG_PATH=/tmp/olr-vllm-metal.log
export OLR_REQUIRE_CANCEL_EVIDENCE=1
```

Then start the host router and create its test key using the preceding instructions, and run
`make sdk-real` and `make bench-real`. The recorded launch also passed
`--gpu-memory-utilization 0.1`, which the server reports as overridden by the KV cache size.
This model uses about 147 KB of KV cache per token, so a 32,768-token context needs 4.8 GB;
the 6 GiB setting holds 43,680 tokens. Coding clients need the larger context: one OpenCode
session reached 18,000 input tokens. The cancellation evidence and log rules are the same as for the CPU backend
below.

### Local vLLM on an ARM64 CPU

The optional [CPU backend Compose file](../deploy/compose/vllm-cpu.yml) runs genuine vLLM
**0.33.0** with **Qwen3-0.6B**, including native OpenAI and Anthropic routes. Both the image
digest and model revision are pinned. It uses eight CPU cores, a 4 GiB KV cache, a 16,384-token
context and a 14 GiB container memory limit; the model download is about 1.4 GiB. The API is
published only on loopback. This works inside Docker's Linux ARM64 VM on Apple Silicon;
it does not use the Mac GPU. See the upstream [ARM CPU installation guide](https://docs.vllm.ai/en/latest/getting_started/installation/cpu/index.html?device=arm).

```sh
docker compose -f deploy/compose/vllm-cpu.yml up -d --wait --wait-timeout 240
export OLR_DIRECT_BASE_URL=http://127.0.0.1:18000
export OLR_UPSTREAM_MODEL=tiny-qwen
export OLR_DIRECT_API_KEY=local-vllm-no-auth
export OLR_HEALTH_PATH=/health
export OLR_DISABLE_THINKING=1
export OLR_CHECK_THINKING=1
```

Then start the host router and create its test key using the preceding instructions. Set
`OLR_VLLM_CPU_BIND` before starting Compose to change the default `0-7` core binding.
The server uses the `hermes` tool parser and `qwen3` reasoning parser. Leave thinking enabled
on the server: the ordinary SDK checks disable it per request, while the explicit thinking
check still exercises it. Embeddings need a separate embedding model and are not covered
by this chat model.

This vLLM version can abort a request without incrementing its Prometheus abort counter
([upstream issue #55634](https://github.com/vllm-project/vllm/issues/55634)). Capture its logs
for the cancellation checks:

```sh
export OLR_VLLM_METRICS_URL="$OLR_DIRECT_BASE_URL/metrics"
export OLR_VLLM_LOG_PATH=/tmp/olr-vllm-cpu.log
docker compose -f deploy/compose/vllm-cpu.yml logs --no-color --no-log-prefix -f vllm \
  > "$OLR_VLLM_LOG_PATH" 2>&1 &
vllm_logs_pid=$!
export OLR_REQUIRE_CANCEL_EVIDENCE=1
make sdk-real
make bench-real
# After testing:
kill "$vllm_logs_pid"
docker compose -f deploy/compose/vllm-cpu.yml stop
```

The server enables request logging with `--max-log-len 0`, so log evidence includes request
IDs without prompt text. A log-based pass requires a **new** `Aborted request(s)` entry
matching that stream's response ID and zero running/waiting requests on the backend. Stale
entries, other request IDs, log replacement/truncation and missing IDs do not count as
evidence. `OLR_VLLM_LOG_PATH` requires the metrics URL too; logs alone do not establish an
idle backend. Without the log option, the abort-counter check remains strict.

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

Quirks found with vLLM:

- The vLLM runs added `--tools Read,Edit,Bash` to keep the tool list short, and set
  `CLAUDE_CODE_MAX_OUTPUT_TOKENS=4096` and `CLAUDE_CODE_MAX_CONTEXT_TOKENS=32768` to match the
  server's context (2048 and 16384 for the 16k-context models).
- Claude Code exits zero and reports success even when the model never called a tool or made
  the wrong edit. Read the tool events with `--output-format stream-json --verbose`.
- The model must emit tool calls in the format the server's parser expects. Qwen3 8B does;
  Qwen2.5 Coder 7B answers in prose and the session ends after one turn.
- Thinking stays disabled as above. vLLM Metal rejects a thinking budget with HTTP 400.

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

Run `opencode run --pure --dir /absolute/path/to/fresh-fixture --model olr/chat-model --format json` with the same prompt as Claude
Code, then independently run `python3 -m unittest -v`. Confirm completed read, edit and bash
tool events, an unchanged test file and a successful original test, rather than relying on
the model's final claim.
Use a compatible provider that sends `/v1/chat/completions`; this router does not serve
`/v1/responses`. Newer OpenCode releases use a different `providers`/`package`/`settings`
schema; consult that release's [provider documentation](https://opencode.ai/docs/providers/).

Quirks found with vLLM:

- `"options": {"reasoningEffort": "none"}` suits the Ollama model. With Qwen3 8B on vLLM,
  remove that line: without thinking the model asks for `/invoice.py` in all four runs, and
  OpenCode rejects the path outside the project and ends the session. With thinking left on,
  three of three runs pass, in 37 to 90 seconds each.
- Set `limit.context` to the server's `--max-model-len`. OpenCode's own prompt and tool list
  take about 11,500 tokens before the first file is read, so a 16,384-token context is tight.
- Pass `--dir` with the fixture path. Launched from a script with only the working directory
  set, OpenCode selected the wrong project and did not find `olr/chat-model`.
- OpenCode exits zero when the session ends early or the model's edit is wrong.

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

The `reasoning_effort` setting applies to the tested Ollama and Qwen3 models; remove it when
unsupported. Without it, Qwen3 spends the 128-token limit on thinking and returns no tool call.

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
