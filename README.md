# open-llm-router

Small open-source LLM router for locally hosted models: OpenAI- and Anthropic-compatible API,
Keycloak SSO (any realm, incl. brokered federated IdPs), per-user API keys, group-based model access,
per-user token quotas and usage view, load-aware balancing across model replicas.

Status: **in development** — OpenAI and Anthropic APIs now forward to replica pools with
incremental streaming, cancellation, weighted balancing, health checks and failover.
Model aliases and `/v1/models` work. Authentication, usage accounting and the portal follow. Requirements in [docs/PLAN.md](docs/PLAN.md),
delivery plan in [docs/WEEK1.md](docs/WEEK1.md), work items in the
[issues](https://github.com/arm2arm/open-llm-router/issues/1).
Stack: Go backend (one static binary), React portal, PostgreSQL, Keycloak.
Development runs fully local with Docker Compose (router, Postgres, two mock LLMs);
local Keycloak follows in #14 and Kubernetes manifests after the pilot.

Authors: A. Khalatyan (AIP), with Hermes agent.

## Development

Needs Go 1.27.3 for host-side development, and Docker with Compose 2.20+ for the stack.
From the repository root:

```sh
docker compose up --build -d   # or: make up (also waits for Postgres)
curl --fail localhost:8080/healthz
make smoke                   # readiness, model listing and backend probes
make sdk                     # official OpenAI + Anthropic SDKs (needs uv)
```

The stack works with built-in local defaults. Optionally copy [.env.example](.env.example) to
`.env` to override ports, credentials or token delays. Compose reads `.env` automatically;
host-side Go commands read only exported environment variables. All published ports bind to
`127.0.0.1`. Postgres data is kept in a named volume across `make down` / `make up`;
`docker compose down --volumes` deletes that local data. Postgres credentials initialize an
empty volume, so changing them later also requires updating the database or recreating the volume.

| Service | Host endpoint | Purpose |
|---|---|---|
| `router` | `http://localhost:8080` | OpenAI + Anthropic APIs, `/v1/models`, health probes |
| `postgres` | `localhost:5432` | database `olr`, user `olr`, public dev password `olr-dev-only` |
| `mock-llm-a` | `http://localhost:8001` | `chat-model`, 20 ms per token |
| `mock-llm-b` | `http://localhost:8002` | `chat-model`, 40 ms per token |

The pool URLs in `config.example.yaml` use service names and container ports. The differing
delays match its 2:1 weights, used by weighted least-outstanding balancing. To try a stream through the router:

```sh
curl -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

Build and check the Go code:

```sh
make lint test   # gofmt, go vet, staticcheck; tests with the race detector
make vuln        # govulncheck (pinned version)
make build       # bin/olr and bin/mock-llm
make down
```

Unit tests live next to their code. HTTP streaming and disconnect tests use
`httptest.NewServer` and real sockets, without requiring a running Compose stack.
Postgres integration tests in `tests/integration` skip when `OLR_TEST_DATABASE_URL` is unset.
With Compose running on the default database port, include them with:

```sh
OLR_TEST_DATABASE_URL='postgres://olr:olr-dev-only@127.0.0.1:5432/olr?sslmode=disable' make test
```

CI runs on pushes and pull requests. It uses the same `make lint`, `make vuln` and `make test`
commands with a Postgres 17 service, while a parallel job builds both runtime images, starts
Compose and runs `make smoke` plus the official SDK checks against the router and both mocks. Jobs have a five-minute timeout; Go modules and build outputs
are cached by `setup-go`.

Without Docker, build both binaries, run two mock processes on ports 8001/8002, and copy the
example config with pool URLs changed to `http://127.0.0.1:8001` and `http://127.0.0.1:8002`.
Then run `OIDC_ISSUER=http://localhost:8081/realms/olr-dev bin/olr serve -config <local-config>`.

The deterministic mock backend speaks the OpenAI and Anthropic protocols and can be run directly
or as its own container:

```sh
bin/mock-llm
docker build --target mock-llm -t olr-mock-llm .
docker run --rm -p 127.0.0.1:8000:8000 olr-mock-llm

# With the mock running, check its wire format through the official SDKs.
OLR_BASE_URL=http://127.0.0.1:8000 OLR_EMBEDDING_MODEL=chat-model uv run tests/clients/check_openai.py
OLR_BASE_URL=http://127.0.0.1:8000 OLR_CHECK_THINKING=1 uv run tests/clients/check_anthropic.py
```

Configure it with `MOCK_MODELS`, `MOCK_TOKENS`, `MOCK_TOKEN_DELAY`, `MOCK_FAIL`, and
`MOCK_THINKING`; the equivalent `X-Mock-*` headers override one request. `GET /_mock/stats`
reports requests and cancelled streams, and `POST /_mock/reset` clears the counters.

The router reads one YAML file, named by `OLR_CONFIG` or `-config`;
[config.example.yaml](config.example.yaml) explains the format. It refuses to start on an unknown
key, on a `${VAR}` that is not set, or on a value that fails validation, and lists every problem
it found. Secrets are read from the environment only (`OLR_DATABASE_URL`,
`OIDC_PORTAL_CLIENT_SECRET`). Logs are JSON lines on stdout with a request ID, and never contain
request or response bodies.

| Path | Content |
|---|---|
| `cmd/olr` | the binary; `olr serve` runs the router |
| `cmd/mock-llm`, `internal/mockllm` | deterministic OpenAI- and Anthropic-compatible test backend |
| `internal/config` | config file: `${VAR}` references, strict decoding, validation |
| `internal/server` | HTTP server: routes, request ID and request log, `/healthz`, `/readyz`, graceful shutdown |
| `internal/proxy`, `internal/apierr` | bounded request bodies, protocol errors, byte-preserving streaming and model listing |
| `internal/pool` | weighted selection, in-flight caps, health checks and passive cooldown |
| `internal/reqid` | the request ID carried in a request's context |
| `deploy/compose` | the local development stack |

Still to come under `internal/`: `db`, `auth`, `usage`, `limits`, `portal`, `metrics`.

## API behavior

The router serves `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
`/v1/messages` and `/v1/messages/count_tokens`. Request bodies are limited to 32 MiB.
Unknown fields and client protocol headers pass through. Client credentials and cookies are
removed; replicas can use their own bearer token via `bearer_token_env` in the config.
Backend URLs must be HTTP(S) origins without credentials, a path prefix or a query.

An alias resolves to its configured model; `upstream_model` optionally changes the name sent
to the backend. Only a rewritten request is serialized again, preserving unknown JSON values.
Responses, including backend errors and SSE bytes, pass through unchanged. Redirects are
returned to the caller. Unsupported model/protocol combinations give a protocol-shaped 404.

Pools choose the healthy replica with the smallest `inflight / weight` and randomize ties.
Slots last until completion or disconnect. Full or unhealthy pools return 503 with `Retry-After: 1`;
there is no queue. `/readyz` needs a healthy replica for every configured model, regardless of
whether its slots are currently full. Health probes use each replica's `health_path` (default
`/health`; use `/` for Ollama), at startup and every 5 seconds with a 2-second timeout:
two failures remove a replica, one success restores it. A connection failure
before response bytes triggers a 10-second cooldown and at most one retry on another replica.
Once response bytes have started, a broken stream is cut off and never replayed.

`timeout_s` bounds the wait for response headers; `idle_timeout_s` (default 30 seconds) bounds
silence between chunks and blocked downstream writes. There is no overall generation timeout.
Client disconnects cancel backend work. Streaming responses flush each read and carry
`X-Accel-Buffering: no`, `Cache-Control: no-cache` and the router's `X-Request-Id`.

For a quick alias check:

```sh
curl http://localhost:8080/v1/models
curl -N http://localhost:8080/v1/messages \
  -H 'Content-Type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":true}'
```

The local stack uses deterministic mock answers. A web portal is planned in later issues.

For real Ollama, vLLM or SGLang backends, see [docs/CLIENTS.md](docs/CLIENTS.md) and
[config.real.example.yaml](config.real.example.yaml). The runbook includes the recorded laptop
results, Claude Code/OpenCode tool sessions, backend cancellation evidence and `make sdk-real`
/ `make bench-real`. The deployed vLLM/SGLang acceptance run remains pending.

## Citation

See [CITATION.cff](CITATION.cff) (GitHub: "Cite this repository").

## License

MIT — see [LICENSE](LICENSE).
