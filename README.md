# open-llm-router

Small open-source LLM router for locally hosted models: OpenAI- and Anthropic-compatible API,
Keycloak SSO (any realm, incl. brokered federated IdPs), per-user API keys, group-based model
access, per-user quotas and rate limits, a self-service portal, and load-aware balancing across
model replicas. One Go binary with an embedded React portal; state in PostgreSQL.

Status: **pilot-ready**. Streaming passthrough of both protocols with cancellation, weighted
balancing, queueing, session affinity and priority classes; API keys and Keycloak access
tokens; group policy with per-user overrides; concurrency, RPM, TPM and day/month quotas
(optionally strict); usage records with timing, retention and user-driven deletion; trusted
forwarders such as Open WebUI; group sync from Keycloak; Prometheus metrics, a Grafana
dashboard and OTLP traces. Security review: [docs/SECURITY_REVIEW.md](docs/SECURITY_REVIEW.md).
Requirements in [docs/PLAN.md](docs/PLAN.md),
work items in the [issues](https://github.com/arm2arm/open-llm-router/issues).

Authors: A. Khalatyan (AIP), with Hermes agent.

## Quickstart (5 minutes)

Needs Docker with Compose 2.20+. Everything runs locally: router, Postgres, Keycloak and two
mock LLM backends; no GPU is needed.

```sh
git clone https://github.com/arm2arm/open-llm-router && cd open-llm-router
docker compose up --build -d --wait
```

1. Open <http://localhost:8080/portal/> and sign in as `staff` / `staff-dev-only`.
2. **API keys → Create key**, give it a name and copy the key.
3. Call the router:

```sh
export OLR_API_KEY=olr-...   # the key you copied
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $OLR_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

or with the OpenAI and Anthropic Python SDKs:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="olr-...")
print(client.chat.completions.create(model="chat-model", messages=[{"role": "user", "content": "hello"}]).choices[0].message.content)

from anthropic import Anthropic
client = Anthropic(base_url="http://localhost:8080", api_key="olr-...")
print(client.messages.create(model="chat-model", max_tokens=64, messages=[{"role": "user", "content": "hello"}]).content[0].text)
```

The backends are deterministic mocks, so the answers are canned text. **Usage** in the portal
shows the calls, **Connect** has snippets for Claude Code, OpenCode and other clients, and
signing in as `admin` / `admin-dev-only` adds the **Admin** page. Without the portal, a key can
also be created on the command line:

```sh
user_id=$(docker compose exec -T router olr admin user add -username pilot -groups staff)
docker compose exec -T router olr admin key create -user "$user_id" -label laptop
```

`docker compose down` stops the stack; `docker compose down --volumes` also deletes its data.
To use real models, point `pool` URLs in a config file at vLLM, SGLang or Ollama servers
([docs/CLIENTS.md](docs/CLIENTS.md), [config.real.example.yaml](config.real.example.yaml)).

## Documentation

| Document | Content |
|---|---|
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | deployment, configuration reference, operations, limits, known limitations |
| [docs/REFERENCE.md](docs/REFERENCE.md) | API, authentication, accounting and limits in detail; tests; code layout |
| [docs/PORTAL_API.md](docs/PORTAL_API.md) | the portal's JSON API |
| [docs/OBSERVABILITY.md](docs/OBSERVABILITY.md) | metrics, dashboard, traces and request logs |
| [docs/CLIENTS.md](docs/CLIENTS.md) | SDKs, Claude Code, OpenCode and real backends |
| [docs/PRIVACY.md](docs/PRIVACY.md) | privacy notice template |
| [docs/SECURITY_REVIEW.md](docs/SECURITY_REVIEW.md) | security review and its findings |
| [docs/PLAN.md](docs/PLAN.md), [docs/WEEK1.md](docs/WEEK1.md) | requirements and delivery plan |

## Development

Host-side development needs Go 1.27.4 and Node 22.23.1+ in addition to Docker. The Compose
stack works with built-in defaults; copy [.env.example](.env.example) to `.env` to change ports
or credentials. All published ports bind to `127.0.0.1`, and every credential in the stack is a
public development value.

| Service | Host endpoint | Purpose |
|---|---|---|
| `router` | `http://localhost:8080` | public home page with portal sign-in, APIs, `/portal/`, protected `/metrics`, health probes |
| `postgres` | `localhost:5432` | database `olr`, user `olr`, public dev password `olr-dev-only` |
| `mock-llm-a` | `http://localhost:8001` | `chat-model`, 20 ms per token |
| `mock-llm-b` | `http://localhost:8002` | `chat-model`, 40 ms per token |
| `keycloak` | `http://localhost:8081` | realms `olr-dev` (users `staff`, `student`, `admin`; password `<user>-dev-only`) and `upstream-idp` brokered into it; dev admin console |

| Command | What it does |
|---|---|
| `make up` / `make down` | build and start the stack and wait for it / stop it |
| `make lint test` | gofmt, go vet, staticcheck; unit tests with the race detector (Postgres tests need `OLR_TEST_DATABASE_URL`, below) |
| `make vuln` | govulncheck |
| `make web-check` | portal lockfile install, lint, type check, build, `npm audit` |
| `make build` | portal assets, then `bin/olr`, `bin/mock-llm`, `bin/olr-loadtest` |
| `make smoke` | readiness, model listing and backend probes against the stack |
| `make sdk`, `make sdk-limits` | official OpenAI and Anthropic SDKs through the router (needs `uv`) |
| `make portal-check`, `make brokered-check` | real Keycloak logins, portal API, SSO logout; federated login and required claim |
| `cd web && npm run test:e2e` | Playwright self-service journey |
| `make loadtest` | isolated 256-stream benchmark with accounting and leak checks |
| `make observability-up` | the stack plus Prometheus, Grafana and Jaeger |

```sh
OLR_TEST_DATABASE_URL='postgres://olr:olr-dev-only@127.0.0.1:5432/olr?sslmode=disable' make test
```

CI runs the same commands on every push and pull request.

## Citation

See [CITATION.cff](CITATION.cff) (GitHub: "Cite this repository").

## License

MIT — see [LICENSE](LICENSE).
