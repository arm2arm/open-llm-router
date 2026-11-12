# open-llm-router — Plan v0.3

Status: **implemented** (first release, 2026-11-11), apart from the production pilot and
multiple router instances. v0.1: 2026-11-06. v0.2: 2026-11-10. v0.3: 2026-11-10. v0.4: 2026-11-11.
Owner: A. Khalatyan. License: MIT.

Site-specific values (hostnames, realms, GPU layout, quotas) are **not** kept in this public repository.
They live in a private deployment file that sets the same configuration keys as `config.example.yaml`.

## 0. Changes

### Decisions of the first release that deviate from this plan (v0.4)

- **Concurrency per key class:** `max_concurrent` limits interactive keys and
  `max_concurrent_batch` batch keys (default: `max_concurrent`), instead of
  `max_concurrent: {interactive, batch}` (§9).
- **Priority:** a model's `priority: {interactive, batch}` plus the group's `priority` is sent as
  the vLLM `priority` and orders the router's queue; separate batch aliases are optional (§8, §9).
- **Full pool:** after `queue_timeout_s` a call gets 503 with `Retry-After`, not 429 (§8).
- **Strict quota:** per group `strict_quota: true` reserves `max_tokens` plus the request size in
  bytes (an upper bound of its input tokens) and requires `max_tokens` (§7).
- **Overrides** apply to the numeric limits and `strict_quota`, not to model access (§7, §10).
- **Admin** is a group flag, `admin: true` (§9, §12).
- **Required claim:** `required_claim: {name, value}` or `OIDC_REQUIRED_CLAIM=name=value`; a string
  claim must equal the value, an array must contain it (§6.1).
- **Forwarders:** `header_map` maps a header to a user attribute (`id`, `username` or `email`);
  `X-OLR-End-User` carries a username. An email shared by several users is unknown; a disabled
  user is always refused. `usage.user_id` keeps the key owner and `usage.forwarded_for` the end
  user; daily totals, quotas and the portal follow the end user (§6.6, §10).
- **Access tokens** create the router user `(issuer, sub)` on first use, with the token's groups.
  **Group sync** never re-enables a user; on Keycloak 26.8+ its client needs the `kc-admin-api`
  scope (§6.4, §6.5).
- **Usage table:** numbered SQL migrations instead of a migration tool; no monthly partitions —
  the retention job deletes by an index on time in batches (§10).
- **One router instance:** counters, the queue and session affinity are in memory (§4).


### Since v0.2 (v0.3)

- Router backend in **Go** (standard-library HTTP server, client and reverse proxy; one static
  binary) instead of Python/FastAPI: streaming every token for many concurrent clients must not
  cost them tokens per second, and one process should use all cores.
- Portal is a **React single-page app** (Vite, TypeScript) served from the same binary and backed
  by a JSON API, instead of server-rendered pages.
- **Per-call tracing** is part of v1: tokens, time to first token and tokens per second are
  recorded for every call, shown to the user, exported as metrics and, optionally, as
  OpenTelemetry spans. Metadata only.
- Users are keyed by **issuer plus `sub`** with a surrogate ID; email is an attribute and is never
  used to match users (§10).
- Users can **delete their own activity records**; an operator can erase a user completely (§13).
- Delivery order, scope cut and current estimates are in [WEEK1.md](WEEK1.md); §15 keeps the
  acceptance tests.

### Since v0.1 (v0.2)

- Whole stack runs **locally with Docker Compose**: router, Postgres, **local Keycloak** with an imported
  test realm, mock LLM backend. Nothing external is needed for development or CI.
- Keycloak is **configuration, not code**: issuer URL, client IDs, claim names and group mapping are
  settings, so the same image works against the local dev Keycloak or any production Keycloak.
- Federated login (e.g. an eduGAIN/community AAI) is handled by **Keycloak identity brokering**;
  the router keeps trusting only Keycloak. The dev stack simulates this with a second "upstream" realm.
- Per-user **token quotas** (daily/monthly) and **concurrency limits** besides rpm/tpm.
- **Portal shows each user their own usage**, recent requests (metadata only) and remaining quota.
- **Load-aware balancing** across heterogeneous replicas (weighted least-outstanding requests),
  replacing round-robin; per-backend in-flight caps; optional session affinity.
- **Priority classes** (interactive vs batch) mapped to vLLM's request `priority`.
- **Trusted forwarders** (e.g. Open WebUI) can attribute usage to their end users via headers.
- Packaging: **Docker image first**; Kubernetes manifests/Helm later from the same image and env vars.
- Removed site-internal hostnames from this document.

## 1. Goal

A small, maintainable router in front of **locally hosted** LLMs that

- speaks **OpenAI** and **Anthropic** protocols to clients (passthrough, no translation),
- authenticates users via **Keycloak** (OIDC) and per-user **API keys**,
- lets users **self-serve**: log in, create/revoke keys, see their usage and limits,
- enforces **per-user quotas, rate and concurrency limits** by group,
- **balances load** over many replicas of the same model, including mixed GPU types,
- records **usage metadata** per user (never prompts or responses),
- **traces every call** (tokens, time to first token, tokens per second) and adds **no measurable
  latency or throughput loss** to token streaming.

### Non-goals (v1)

- External/cloud providers, billing in money, protocol translation.
- Prompt/response storage, semantic cache, guardrails, MCP gateway.
- Full tracing UI: the router exports metadata spans over OpenTelemetry to an external backend.

## 2. Why build instead of an existing gateway

| | open-llm-router | LiteLLM (free) | Bifrost (free) | New API (AGPLv3) |
|---|---|---|---|---|
| Self-service SSO login for all users | yes (Keycloak) | UI SSO free for ≤ 5 users | Enterprise only | yes (OAuth/OIDC) |
| Per-user quotas + own usage view | yes | budgets yes; self-service needs SSO | budgets yes; no user login | yes |
| OpenAI + Anthropic passthrough | yes (vLLM native) | translates | translates | Claude relay; untested with vLLM |
| License | MIT | MIT core + enterprise dir | Apache-2.0 core + enterprise | AGPLv3 + attribution terms |
| Size / control | small Go service + React portal, ours | large | large | large |

Fallback if schedule slips: New API can provide a pilot within about a day, accepting AGPLv3 and an
unverified Keycloak/Anthropic path.

## 3. Facts and assumptions

Verified:
- vLLM ≥ 0.28 serves OpenAI `/v1/chat/completions`, `/v1/models` and Anthropic `/v1/messages`
  (text, `tool_use`, SSE streaming; responses may contain `thinking` blocks). Tested on 0.28.
- vLLM has a priority scheduler (`--scheduling-policy priority`; per-request `priority` field).
- Open WebUI can forward user identity headers (`ENABLE_FORWARD_USER_INFO_HEADERS=true` →
  `X-OpenWebUI-User-Id`, `-Email`, `-Name`).
- Measured on one GPU server: independent replicas give more total throughput than one
  tensor-parallel instance (2 × 1-GPU ≈ 3,600 tok/s vs TP2 ≈ 2,960 tok/s at 64 users, dense 27B BF16);
  per-replica speed differs up to ~3× between GPU types. Round-robin would overload slow replicas.

To verify early (M1):
- `/v1/messages` on the vLLM version actually deployed; SGLang backends may lack it → per-model
  `protocols` setting.
- Claude Code / opencode / agent streaming quirks (beta headers, prompt-caching fields).

## 4. Architecture

```
                 ┌──────────── Keycloak realm ─────────────┐   (dev: local container;
 browser ──OIDC──┤  optional broker → upstream IdP / AAI   │    prod: any Keycloak)
                 ▼                                         │ JWKS
        /portal (login, keys, my usage, my limits)         │
                                                           ▼
 client ── Bearer <api-key | Keycloak token> ──>  ROUTER (Go, one binary, stateless)
  OpenAI SDK, Anthropic SDK, Claude Code,           │ authn → authz(model) → quota/limits
  opencode, agents, Open WebUI (trusted forwarder)  │ → pick replica (weighted least-outstanding)
                                                    │ → stream passthrough, cancel on disconnect
                                                    │ → usage record
                                                    ▼
                          model pools: 1..n vLLM replicas each (interactive / batch)
        Postgres: users, keys, usage, quotas     Prometheus /metrics (aggregate labels only)
```

Router is a single container image; state in Postgres. v1 runs one router instance; for >1
instance, per-user concurrency counters move to Redis (or Postgres advisory locks).

## 5. API surface

| Path | Protocol | Notes |
|---|---|---|
| `POST /v1/chat/completions`, `/v1/completions` | OpenAI | stream + non-stream |
| `POST /v1/embeddings` | OpenAI | if an embedding model is configured |
| `GET  /v1/models` | OpenAI | only models the caller may use |
| `POST /v1/messages`, `/v1/messages/count_tokens` | Anthropic | stream, tools, thinking |
| `GET  /v1/me/usage` | JSON | caller's quota, used, reset times (for agents/scripts) |
| `GET  /healthz`, `/readyz`; `/metrics` | ops | metrics not exposed publicly |
| `/portal/*` | web | React app: keys, usage, limits, client snippets; login routes; JSON under `/portal/api/*` |

Auth headers: `Authorization: Bearer …` and `x-api-key: …` (Anthropic SDK / Claude Code).

## 6. Identity and authorization

1. **Keycloak is pluggable.** Settings: `OIDC_ISSUER`, `OIDC_PORTAL_CLIENT_ID/SECRET`,
   `OIDC_AUDIENCE`, `OIDC_GROUPS_CLAIM` (default `groups`), `OIDC_USERNAME_CLAIM`, optional
   `OIDC_REQUIRED_CLAIM`/value (e.g. an affiliation or entitlement). Discovery via
   `.well-known/openid-configuration`; JWKS cached and refreshed on unknown `kid`.
2. **Federated login** (community AAI, institutional IdP) is configured in Keycloak as an identity
   provider; access restriction (affiliation/entitlement → group) is done with Keycloak mappers. The
   router additionally enforces `OIDC_REQUIRED_CLAIM` if set (defence in depth).
3. **API keys**: `olr-` + 32 random bytes (base62); stored as SHA-256 hash + prefix; shown once;
   bound to the user (issuer + `sub`, see §10); label, optional expiry; class `interactive` or `batch`.
4. **Keycloak access tokens** accepted for services (client-credentials): check `iss`, `aud`, `exp`.
5. **Groups → policy** (models, quotas, limits, priority) from config.
   Groups for API-key users are refreshed at portal login and by a periodic sync job (service
   account with `view-users`); disabled/removed users get their keys disabled.
6. **Trusted forwarders**: a key flagged `trusted_forwarder` may set `X-OLR-End-User` (or a mapped
   header such as `X-OpenWebUI-User-Email`). The router resolves it to a known user; quotas and
   usage apply to that user; unknown users fall back to the forwarder's own policy or are rejected
   (configurable). Headers from non-trusted keys are ignored.

Keycloak clients per realm: `olr-portal` (confidential, auth code + PKCE, groups mapper),
`olr-api` (audience for service tokens), `olr-sync` (service account, read-only users/groups).

## 7. Quotas and limits

- Per group, overridable per user: `tokens_per_day`, `tokens_per_month`, `rpm`, `tpm`,
  `max_concurrent` (separately for interactive/batch keys), allowed models.
- Enforcement order: auth → model allowed → concurrency slot → rpm/tpm bucket → quota remaining.
  Rejections: `429` with `Retry-After` and a JSON body naming the limit and reset time
  (OpenAI and Anthropic error shapes respectively).
- Accounting after completion from backend `usage` (stream: final chunk), counting input plus
  output tokens. Already admitted calls can overshoot a quota. With finite concurrency and
  bounded calls, the bound is their total input/output budget; `max_concurrent × max_tokens`
  bounds only output. Optional pre-reservation for strict groups follows in #27.
- Windows: day/month in a configurable timezone; counters in Postgres (`UPDATE … RETURNING`).

## 8. Routing and load balancing

- `models.<name>.pool` lists replicas with `weight` (relative throughput) and `max_inflight`.
- Selection: among healthy replicas below `max_inflight`, minimise `inflight / weight`;
  ties broken randomly. If all are full: queue up to `queue_timeout_s`, then `429`/`503`.
- Health: active `GET /health` every few seconds + passive (connect errors/5xx → cool-down).
  No retry after the first streamed byte; before that, one retry on a different replica.
- Optional **session affinity**: hash of `X-Session-Id` (or forwarder chat ID) prefers the same replica
  for prefix-cache reuse, unless it is overloaded.
- **Priority**: key class / group maps to vLLM `priority` (backends started with
  `--scheduling-policy priority`); batch pools can be separate model aliases.
- `aliases` map client default names (e.g. Claude Code model IDs) to local models.

## 9. Configuration (`config.example.yaml` in Git; site file private)

```yaml
oidc:
  issuer: ${OIDC_ISSUER}            # dev: http://keycloak:8080/realms/olr-dev
  portal_client_id: olr-portal
  audience: olr-api
  groups_claim: groups
  required_claim: null             # e.g. {name: affiliation, contains: "member@example.org"}
models:
  chat-model:
    protocols: [openai, anthropic]
    timeout_s: 900
    pool:
      - {url: http://backend-a:8000, weight: 2.0, max_inflight: 64}
      - {url: http://backend-b:8000, weight: 1.0, max_inflight: 32}
  chat-model-batch:
    protocols: [openai, anthropic]
    priority: 10
    pool: [{url: http://backend-c:8000, weight: 1.0, max_inflight: 64}]
groups:
  staff:    {models: ["*"], tokens_per_day: 2000000, rpm: 60,  max_concurrent: {interactive: 4, batch: 8}}
  students: {models: [chat-model], tokens_per_day: 300000, rpm: 20, max_concurrent: {interactive: 2, batch: 2}}
  admins:   {models: ["*"], admin: true}
forwarders:
  header_map: {X-OpenWebUI-User-Email: email}
  unknown_user: reject            # or: forwarder_policy
aliases: {claude-sonnet-4-5: chat-model}
```

## 10. Data model (Postgres)

- `users(id, issuer, sub, username, email, groups[], enabled, synced_at)` — unique on `(issuer, sub)`
- `api_keys(id, user_id, prefix, hash, label, class, trusted_forwarder, created, last_used, expires, revoked)`
- `sessions(id_hash, user_id, created, expires)` — portal sessions
- `usage(ts, key_id, user_id, forwarded_for, model, replica, protocol, status, input_tokens,
  output_tokens, latency_ms, ttfb_ms)` — monthly partitions; retention configurable (default 90 days)
- `usage_daily(day, user_id, model, requests, input_tokens, output_tokens)` — portal and quotas
- `quota_overrides(user_id, field, value, reason, set_by, set_at)`

User identity: the OIDC `sub` is unique only within one issuer, so `(issuer, sub)` identifies a
login and the surrogate `id` is what other tables reference. `email` is an attribute: not unique
and never used to match or merge users. One person with several identity providers is linked in
Keycloak (account linking), so they arrive with one `sub`. Deleting a user removes their keys,
sessions and usage.

## 11. Router behaviour (the hard parts)

- Byte-for-byte SSE passthrough, no buffering; proxy buffering disabled at ingress.
- Cancel the upstream request on client disconnect (stops GPU work).
- Timeouts: connect 5 s, read up to `timeout_s`; ingress timeouts matched.
- Usage from non-stream `usage`; streams: inject `stream_options.include_usage=true` (OpenAI) if
  absent and strip the extra chunk only if the client did not ask for it; Anthropic `message_delta.usage`.
- Body size limit (e.g. 32 MB); model allow-list checked before forwarding; unknown fields forwarded.
- Never log prompts/responses; structured logs with request ID only.
- Per call: time to first token, total duration and output tokens per second are measured in the
  proxy and stored with the usage record.
- Overhead budget, verified by the load test: less than 5 ms added to the time to first token
  (p95), and tokens per second within 2 % of a direct connection at 256 concurrent streams.

## 12. Portal

Single-page app (Vite, React, TypeScript, Tailwind CSS, shadcn/ui), built to static files and
embedded in the router binary. It talks to a session-authenticated JSON API under `/portal/api`.
The backend does the OIDC login, so no token reaches the browser:
- **Keys**: create (label, class, expiry), list (prefix, last used), revoke.
- **My usage**: today / this month per model with average time to first token and tokens per
  second, chart of last 30 days, remaining quota and reset time.
- **My requests**: last 200 requests (time, model, tokens, latency, status) — metadata only.
- **Privacy**: delete my request records; remove revoked keys for good.
- **Connect**: copy-paste snippets (OpenAI SDK, Anthropic SDK/Claude Code, opencode, curl).
- **Admin** (group `admins`): users, per-user overrides, group totals, replica health.

## 13. Observability and privacy

- Prometheus: requests, tokens, latency, time to first token, tokens per second, queue time,
  in-flight per **model / replica / group** (no per-user labels). Backend vLLM `/metrics` scraped
  alongside. Grafana dashboard in repo.
- Per-user detail only in Postgres, visible to the user and admins.
- OpenTelemetry: one span per call (model, replica, tokens, time to first token, tokens per
  second; no content, and no user identifier unless a site enables it), exported over OTLP when an
  endpoint is configured. An incoming `traceparent` is passed on to the backend.
- Privacy notice template in docs; retention setting enforced by a nightly job.
- Erasure (GDPR Art. 17): users delete their own per-request records in the portal at any time.
  Daily totals stay for quota enforcement and accounting and expire after a separate, configurable
  period. An operator can erase a user completely (keys, sessions, records, totals). Deleting
  records does not reset quotas.

## 14. Packaging and deployment

**Development / pilot: Docker Compose** (`deploy/compose/`):

| Service | Image | Purpose |
|---|---|---|
| `router` | built from `Dockerfile` (multi-stage: Node builds the portal, Go builds one static binary; distroless runtime) | the router + portal |
| `postgres` | postgres:17 | state |
| `keycloak` | quay.io/keycloak/keycloak (dev mode) | realm `olr-dev` imported from `deploy/keycloak/olr-dev-realm.json`: clients, groups `staff/students/admins`, test users |
| `keycloak` realm `upstream-idp` | same container | simulates a federated IdP; brokered into `olr-dev` to test the AAI flow and claim restriction |
| `mock-llm` | small Go program (`cmd/mock-llm`) | OpenAI + Anthropic responses, streaming, tool calls, configurable delay, `usage` |
| `vllm` (optional profile) | vllm/vllm-openai | real backend on a GPU host; or point `pool` URLs at existing servers |

- `docker compose up` gives a working system on `http://localhost:8080` (router) and
  `http://localhost:8081` (Keycloak) with test users; `make test` runs unit + integration tests against it.
- **Switching Keycloak** = changing env (`OIDC_ISSUER`, client ID/secret) — no code or image change.
- Container data paths are configurable (host bind mounts), e.g. image/cache storage on a dedicated disk.

**Kubernetes** (kustomize, `deploy/k8s/`, verified on kind; see the runbook) — the same image and env vars; manifests in `deploy/k8s/`
(Deployment, Service, Ingress with buffering off and long timeouts, Secret, external Postgres).
Added after the Compose pilot is accepted (M6).

**Model backends** (outside this repo's scope, documented as an example): one vLLM instance per
replica (systemd unit or container), listening on an internal interface, firewalled to the router.

## 15. Milestones and acceptance tests

| # | Milestone | Done when | Est. |
|---|---|---|---|
| M0 | Compose dev stack: router skeleton, Postgres, Keycloak realm import, mock-llm, CI | `docker compose up` + `make test` green on a clean machine | 0.5 d |
| M1 | Passthrough both protocols, pools, weighted least-outstanding, health, cancel | OpenAI + Anthropic SDK: text, stream, tools pass vs mock-llm and one real vLLM; disconnect cancels upstream; slow replica receives proportionally fewer requests | 1 d |
| M2 | Auth: API keys, Keycloak tokens, groups, forwarders | invalid/expired/revoked → 401; forbidden model → 403; `/v1/models` filtered; forwarded user attributed; untrusted header ignored | 0.5 d |
| M3 | Quotas, rpm/tpm, concurrency, usage records, `/v1/me/usage` | token counts equal backend `usage`; 429 with reset time at each limit; concurrent overshoot within bound | 0.5 d |
| M4 | Portal (keys, my usage, my requests, snippets, admin) + brokered login | test user logs in through `upstream-idp` → `olr-dev`, creates key, uses it from Claude Code, opencode, curl; user without required claim is refused | 1 d |
| M5 | Hardening: security review, load test, metrics, runbook, privacy notice | independent review findings closed; 256 concurrent streams through router without errors; Grafana shows pools | 1–2 d |
| M6 | Pilot on a production Keycloak + real GPU pools; then Kubernetes manifests | pilot users active for 2 weeks; switch to production IdP was config-only | — |

The estimates in this table are superseded by [WEEK1.md](WEEK1.md): about 7 working days to a
pilot-ready first release with a reduced scope, and about 6–7 more for the scope deferred there.
Production Keycloak client creation, IdP federation approval, DNS/TLS and firewall rules run in
parallel and set the actual go-live date.

## 16. Risks

- Single maintainer → small codebase, tests first, runbook.
- Two languages (Go, TypeScript) → standard library first in Go; portal kept to a few screens.
- Client quirks (Anthropic beta headers, prompt-caching fields) → forward unknown fields; test each client.
- Quota overshoot under concurrency → bounded by `max_concurrent`; strict mode reserves `max_tokens`.
- Keycloak/claim naming differs per site → all claim names configurable; tested with the brokered dev realm.
- Security of a self-built auth layer → independent review (M5), key hashing, constant-time compare,
  no secrets in logs, dependency pinning.

## 17. Open questions

1. Production Keycloak realm and who creates clients and the IdP federation.
2. Production hostname, TLS termination and where the Compose pilot runs.
3. Default quotas and concurrency per group.
4. Which models/pools at start (interactive vs batch split).
5. Retention periods (per-request records, default 90 days; daily totals for accounting), privacy
   notice owner, and who answers erasure requests.
6. When to move from Compose to Kubernetes.
