# open-llm-router

Small open-source LLM router for locally hosted models: OpenAI- and Anthropic-compatible API,
Keycloak SSO (any realm, incl. brokered federated IdPs), per-user API keys, group-based model access,
per-user token quotas and usage view, load-aware balancing across model replicas.

Status: **in development** — so far the router starts, checks its configuration and answers
health probes; it does not serve the API yet. Requirements in [docs/PLAN.md](docs/PLAN.md),
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
make smoke                   # probe the router and both mock backends
```

The stack works with built-in local defaults. Optionally copy [.env.example](.env.example) to
`.env` to override ports, credentials or token delays. Compose reads `.env` automatically;
host-side Go commands read only exported environment variables. All published ports bind to
`127.0.0.1`. Postgres data is kept in a named volume across `make down` / `make up`;
`docker compose down --volumes` deletes that local data. Postgres credentials initialize an
empty volume, so changing them later also requires updating the database or recreating the volume.

| Service | Host endpoint | Purpose |
|---|---|---|
| `router` | `http://localhost:8080` | `/healthz` and `/readyz`; API forwarding follows in #5 |
| `postgres` | `localhost:5432` | database `olr`, user `olr`, public dev password `olr-dev-only` |
| `mock-llm-a` | `http://localhost:8001` | `chat-model`, 20 ms per token |
| `mock-llm-b` | `http://localhost:8002` | `chat-model`, 40 ms per token |

The pool URLs in `config.example.yaml` use service names and container ports. The differing
delays match its 2:1 weights, ready for balancing in #6. To try a stream directly against a mock:

```sh
curl -N http://localhost:8001/v1/chat/completions \
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
Compose and runs `make smoke`. Jobs have a five-minute timeout; Go modules and build outputs
are cached by `setup-go`.

Without Docker:

```sh
OIDC_ISSUER=http://localhost:8081/realms/olr-dev bin/olr serve -config config.example.yaml
```

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
| `internal/reqid` | the request ID carried in a request's context |
| `deploy/compose` | the local development stack |

Still to come under `internal/`: `proxy`, `pool`, `db`, `auth`, `usage`, `limits`, `portal`,
`metrics`.

## Citation

See [CITATION.cff](CITATION.cff) (GitHub: "Cite this repository").

## License

MIT — see [LICENSE](LICENSE).
