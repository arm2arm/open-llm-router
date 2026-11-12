# open-llm-router

**One endpoint for your team's locally hosted LLMs.** Connect OpenAI and Anthropic
clients to Ollama, vLLM or SGLang pools, give people access through groups, and keep
usage and quotas visible. One Go binary serves both the APIs and the React portal;
PostgreSQL holds identities, settings and usage.

[![CI](https://github.com/arm2arm/open-llm-router/actions/workflows/ci.yml/badge.svg)](https://github.com/arm2arm/open-llm-router/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-0f766e.svg)](LICENSE)

- **Connect your model pool.** Balance requests across replicas, stream responses,
  and edit backend connections with model detection in Admin.
- **Decide who can use it.** Keycloak SSO or local login, per-user API keys,
  group-based model access, and quota, rate and concurrency limits.
- **See what is happening.** A self-service usage portal, request timing, replica
  health, group totals, private conversation and stream traces, and
  Prometheus/Grafana/OpenTelemetry integration.

![Admin overview showing endpoint configuration and healthy model replicas](docs/images/admin-overview.png)

*The actual Admin portal, shown with disposable demo accounts and synthetic usage.
The two mock backends stand in for GPU servers. [About these screenshots](docs/images/README.md).*

## How it works

```mermaid
flowchart LR
    Clients["Your clients<br/>OpenAI / Anthropic APIs"] --> Router["open-llm-router<br/>Authenticate · authorize · limit · route"]
    Router --> A["Model replica A<br/>Ollama / vLLM / SGLang"]
    Router --> B["Model replica B<br/>Ollama / vLLM / SGLang"]
    Portal["Web portal<br/>Users + administrators"] --> Router
    Identity["Keycloak SSO<br/>or local accounts"] --> Router
    Router --> DB[("PostgreSQL<br/>Keys · settings · usage")]
    style Router fill:#0f766e,color:#fff,stroke:#115e59
    style Portal fill:#ecfdf5,stroke:#99c9bb
    style Identity fill:#ecfdf5,stroke:#99c9bb
    style DB fill:#f1f5f9,stroke:#94a3b8
```

A client sends a public model name such as `chat-model`. The router checks its
user's access and limits, selects a healthy replica with free capacity, and maps
the name to the backend's model. It forwards the native API payload and streamed
events, including thinking, tool calls and image inputs when the backend supports
them. The router records usage metadata and timing. [Personal request tracing](docs/REQUEST_TRACING.md)
is enabled by default: users can inspect text, reasoning, tools and stream events, disable
capture, and choose 1–30 days of retention (7 days by default).

## A look inside

| Your usage and allowance | Backend connections |
|---|---|
| [![Usage page with daily and monthly allowances and a 30-day token chart](docs/images/portal-usage.png)](docs/images/portal-usage.png) | [![Endpoint editor with detected upstream models, protocol settings and replica connections](docs/images/endpoint-editor.png)](docs/images/endpoint-editor.png) |
| See token totals, remaining quota and request performance. Create your own API keys and use **Connect** for client setup. | Add or edit replicas, detect upstream model names, test connections, and apply or restore saved revisions. |

| Request observability | Conversation inspection |
|---|---|
| [![My requests with live updates, logging controls and an agent run grouped with its call timeline](docs/images/portal-requests.png)](docs/REQUEST_TRACING.md) | [![Conversation trace with input messages, tool arguments and results, and assistant reasoning](docs/images/request-trace-conversation.png)](docs/REQUEST_TRACING.md#inspect-a-call) |
| Watch requests live, group agent runs, and find streaming, failed, tool-calling or traced calls. | Inspect text, thinking and [tool calls paired with results](docs/REQUEST_TRACING.md#inspect-a-call), the timing breakdown, the stream timeline and the [raw request as sent and forwarded](docs/REQUEST_TRACING.md#inspect-a-call). |

*Observability examples use synthetic requests through the mock backends. The
404 row is an intentional unknown-model request; timings are illustrative.*

<details>
<summary>Keycloak and local sign-in</summary>

![Sign-in page offering Keycloak and an independent local account login](docs/images/sign-in.png)

Keycloak is the default choice when configured. A protected local administrator
can still sign in when Keycloak is unavailable. Local users complete a required
password change on first login.

</details>

## Current status

**Pilot-ready for a single router process.** The features below are implemented;
the production pilot acceptance remains tracked in [#21](https://github.com/arm2arm/open-llm-router/issues/21)
and the [pilot checklist](docs/PILOT.md).

| Area | Available today |
|---|---|
| Routing | OpenAI and Anthropic native APIs; SSE streaming and cancellation; weighted balancing, queueing, session affinity and priority classes |
| Accounts and keys | Keycloak SSO, including brokered identity providers; independent local login and protected admin with an operator recovery CLI; Keycloak connection drafts, checks and test sign-in in Admin; local user management; expiring per-user keys and optional encrypted Admin key copy |
| Access and limits | Group model grants; per-user overrides in Admin; concurrency, RPM, TPM and daily/monthly token quotas, including strict reservation |
| Endpoint administration | Add/edit/remove replicas for existing logical models; remote model detection; health tests; persisted revisions, live application and restore |
| Usage and operations | Usage history and timing; personal text, reasoning, tool and stream traces with user-controlled capture and 7-day default retention; user-driven deletion; Open WebUI trusted forwarding; Keycloak group sync; Prometheus metrics, Grafana dashboard and OTLP traces |
| Deployment | Local Compose demo, real-backend examples, production pilot Compose, [Helm chart](charts/open-llm-router) and [Kubernetes manifests](deploy/k8s) |

**Still planned:** activating Keycloak settings from Admin without a restart; creating logical models and
aliases in Admin; editing group policies in Admin;
and shared limits/live configuration across multiple router processes. Logical
names, aliases and group defaults currently use YAML, while Keycloak settings use
YAML/environment. See the [capability audit and milestones](docs/ADMIN_FEATURE_PLAN.md)
and [security review](docs/SECURITY_REVIEW.md) for scope and limitations.

## Quickstart (5 minutes)

For practical Docker Compose and Kubernetes/Helm installation, first login,
real-backend setup and upgrades, follow the [installation guide](docs/INSTALL.md).

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

For Ollama running on the same laptop as Docker Desktop, set these values in the root
`.env` file, choosing a model already installed in `ollama list`:

```dotenv
OLR_CONFIG_FILE=../../config.ollama.example.yaml
OLLAMA_BASE_URL=http://host.docker.internal:11434
OLLAMA_MODEL=qwen3.8:27b-mlx
```

Run `docker compose up -d --wait router` to apply them. The portal, login and access
policies stay available; `chat-model` and the selected Ollama model name both route to
that real model. Config paths are relative to `deploy/compose`; absolute paths also work.

## Documentation

| Document | Content |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | step-by-step Compose and Kubernetes/Helm install, first login, real backends, upgrades and troubleshooting |
| [Helm chart](charts/open-llm-router) | one-release demo stack, initial passwords/keys, real endpoints and an HTTPS production example |
| [docs/ADMIN_GUIDE.md](docs/ADMIN_GUIDE.md) | administrator setup, endpoint connections, groups, user limits and current UI boundaries |
| [docs/ADMIN_FEATURE_PLAN.md](docs/ADMIN_FEATURE_PLAN.md) | implementation audit and plan for protected local admin, dual login, Keycloak settings and endpoint/policy editing |
| [docs/RUNBOOK.md](docs/RUNBOOK.md) | deployment, configuration reference, operations, limits, known limitations |
| [docs/REFERENCE.md](docs/REFERENCE.md) | API, authentication, accounting and limits in detail; tests; code layout |
| [docs/PORTAL_API.md](docs/PORTAL_API.md) | the portal's JSON API |
| [docs/OBSERVABILITY.md](docs/OBSERVABILITY.md) | metrics, dashboard, traces and request logs |
| [docs/REQUEST_TRACING.md](docs/REQUEST_TRACING.md) | illustrated request viewer, conversation and stream inspection, personal retention and capture limits |
| [docs/CLIENTS.md](docs/CLIENTS.md) | SDKs, Claude Code, OpenCode and real backends |
| [docs/PRIVACY.md](docs/PRIVACY.md) | privacy notice template |
| [docs/SECURITY_REVIEW.md](docs/SECURITY_REVIEW.md) | security review and its findings |
| [docs/PLAN.md](docs/PLAN.md), [docs/WEEK1.md](docs/WEEK1.md) | requirements and delivery plan |

Start with the [administrator guide](docs/ADMIN_GUIDE.md) for protected-admin
bootstrap, local users, external Keycloak setup, backend connections and limits.

## Development

Host-side development needs Go 1.27.4 and Node 22.23.1+ in addition to Docker. The Compose
stack works with built-in defaults; copy [.env.example](.env.example) to `.env` to change ports
or credentials. All published ports bind to `127.0.0.1`, and every credential in the stack is a
public development value.

| Service | Host endpoint | Purpose |
|---|---|---|
| `router` | `http://localhost:8080` | public home page with documentation, project credits and portal sign-in; APIs, `/portal/`, protected `/metrics`, health probes |
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

## About and acknowledgments

open-llm-router is developed by [Arman Khalatyan](https://github.com/arm2arm) at
the [Leibniz Institute for Astrophysics Potsdam (AIP)](https://www.aip.de/), using
tools including Hermes, OpenAI Codex and Anthropic Claude Code.

Security checks were performed using OpenAI’s Daybreaker model.

Development of this project at AIP was funded by the **German Federal Ministry of
Research, Technology and Space (BMFTR)** through the **ErUM-Data** programme as part
of **[Physics-LLM](https://physics-llm.erumdatahub.de/)**. See
[AIP's Physics-LLM project description](https://jobs.aip.de/rec042/) and the
[ErUM-Data-Hub project directory](https://wiki.erumdatahub.de/bin/view/DIG-UM/ErUM-Data%20Projects/).

## Citation

If you use open-llm-router in research or a publication, please cite the software:

> Khalatyan, Arman. *open-llm-router* [Computer software]. Leibniz Institute for
> Astrophysics Potsdam (AIP). https://github.com/arm2arm/open-llm-router

[CITATION.cff](CITATION.cff) supplies machine-readable author and repository
metadata. Use GitHub's **Cite this repository** menu to export BibTeX or APA, and
include the version or commit you used for reproducibility.

## License

MIT — see [LICENSE](LICENSE).
