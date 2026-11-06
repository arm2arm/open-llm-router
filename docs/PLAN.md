# open-llm-router — Plan

Status: **plan, not implemented** (2026-11-06). Owner: A. Khalatyan (AIP). Discussion: Monday 2026-11-09.

## 1. Goal

A small, maintainable, open-source router in front of AIP's **local** LLMs that

- speaks both **OpenAI** and **Anthropic** protocols to clients,
- authenticates users with **AIP Keycloak** (SSO) and per-user **API keys**,
- routes each call by `model` to one or more vLLM backends,
- records who used which model and how many tokens.

### Non-goals (v1)

- No external providers (OpenAI/Anthropic cloud), no billing in money.
- No protocol translation: vLLM ≥ 0.28 serves both APIs natively (see §3).
- No prompt/response storage (privacy); only metadata and token counts.
- No semantic cache, guardrails, MCP gateway.

## 2. Why build instead of Bifrost

| | Own router | Bifrost (free) |
|---|---|---|
| Keycloak SSO | built in | Enterprise only → needs oauth2-proxy + separate portal |
| Anthropic ↔ OpenAI | not needed (vLLM native) | translates (not needed here) |
| Budgets, dashboard, logs UI | minimal (own) | rich |
| Size / control | ~1–2k lines, fully ours | large Go codebase, vendor roadmap |
| Maintenance | ours | upstream |

Bifrost v2.4.0 keeps running at `bifrost.kube.aip.de` until the router is accepted; then decide whether to retire it.

## 3. Verified facts (2026-11-05/06)

- AIP vLLM (`aip-best`) runs **vLLM 0.28.0**. Native `/v1/messages` works: plain text,
  **tool_use** (`get_time` → `{"tz":"Europe/Berlin"}`, `stop_reason: tool_use`), and **SSE streaming**
  (`message_start … content_block_delta … message_stop`). Responses may contain `thinking` blocks.
- OpenAI `/v1/chat/completions` and `/v1/models` work on the same server.
- Keycloak at `keycloak.kube.aip.de` (realm `master` found; **AIP user realm still to confirm**).

## 4. Architecture

```
                 ┌──────────── Keycloak (AIP realm) ─────────────┐
 browser ──OIDC──┤                                               │
                 ▼                                               │ JWKS
           /portal (login, create/revoke keys, usage, snippets)  │
                                                                 ▼
 client ──Bearer <api-key | Keycloak access token>──>  ROUTER  (FastAPI, async)
   OpenAI SDK, Anthropic SDK,                            │  auth → authorize(model) → pick backend
   Claude Code, opencode, curl                           │  stream passthrough, cancel on disconnect
                                                         │  usage record (tokens, latency, status)
                                                         ▼
                                     vLLM backends (per model, 1..n replicas)
                                                         │
                                     Postgres: keys, usage │ Prometheus /metrics
```

Single stateless container (horizontally scalable); state in Postgres.

## 5. API surface

| Path | Protocol | Notes |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI | stream + non-stream |
| `POST /v1/completions` | OpenAI | |
| `POST /v1/embeddings` | OpenAI | if an embedding model is configured |
| `GET  /v1/models` | OpenAI | only models the caller may use |
| `POST /v1/messages` | Anthropic | stream + non-stream, tools, thinking |
| `POST /v1/messages/count_tokens` | Anthropic | forwarded |
| `GET  /healthz`, `/readyz`, `/metrics` | ops | metrics internal only |
| `/portal/*` | web | Keycloak login, key management |

Auth headers accepted: `Authorization: Bearer …` (both SDKs) and `x-api-key: …` (Anthropic SDK/Claude Code).

Client config examples (portal shows them):

```bash
export OPENAI_BASE_URL=https://llm.aip.de/v1        OPENAI_API_KEY=aip-…
export ANTHROPIC_BASE_URL=https://llm.aip.de        ANTHROPIC_API_KEY=aip-…   # Claude Code
```

## 6. Authentication & authorization

1. **API keys** (`aip-` + 32 random bytes, base62). Stored as SHA-256 hash + prefix; shown once.
   Bound to a Keycloak user (`sub`, `preferred_username`), optional expiry, label.
2. **Keycloak access tokens** (for services / client-credentials): verified offline against realm JWKS
   (cached), check `iss`, `aud=llm-router`, `exp`.
3. **Authorization** by Keycloak **groups** → model allow-list and rate limits (config file, §7).
   For API keys, groups are refreshed at portal login and by the nightly sync.
4. **Revocation:** user revokes in portal; nightly job disables keys of users disabled/removed in Keycloak
   (read-only service account `view-users`).

Keycloak clients:

| Client | Type | Purpose |
|---|---|---|
| `llm-portal` | confidential, auth-code + PKCE | portal login; mapper `groups` in ID/access token |
| `llm-router` | bearer-only / audience | audience for service tokens |
| `llm-router-sync` | confidential, service account | nightly user/group sync (realm-management: view-users) |

## 7. Configuration (`config.yaml`, in Git; secrets via env/K8s Secret)

```yaml
models:
  aip-best:
    backends: ["http://vllm-a:8000", "http://vllm-b:8000"]   # round-robin, health-checked
    protocols: [openai, anthropic]
    timeout_s: 900          # reasoning models can think for minutes
    max_concurrency: 64     # per router instance, protects the GPU
groups:
  aip-staff:   {models: ["*"],        rpm: 60,  tpm: 200000}
  students:    {models: ["aip-best"], rpm: 20,  tpm: 50000}
  admins:      {models: ["*"],        rpm: 600, admin: true}
aliases:
  claude-sonnet-4-5: aip-best   # optional: lets Claude Code default model names resolve locally
```

## 8. Data model (Postgres)

- `api_keys(id, user_sub, username, prefix, hash, label, created, last_used, expires, revoked)`
- `users(sub, username, email, groups[], enabled, synced_at)`
- `usage(ts, key_id|null, user_sub, model, backend, protocol, status, input_tokens, output_tokens, latency_ms, ttfb_ms)`
  — partitioned by month; daily aggregates for the portal.

## 9. Router behaviour details (the hard parts)

- **Streaming passthrough** byte-for-byte (SSE), no buffering; ingress buffering off.
- **Cancel upstream on client disconnect** so vLLM stops generating (saves GPU).
- **Timeouts**: connect 5 s, read up to `timeout_s`; ingress/proxy timeouts raised to match.
- **Token accounting**: non-stream from `usage`; stream from final chunk
  (OpenAI: request `stream_options.include_usage=true` injected if absent; Anthropic: `message_delta.usage`).
- **Backend selection**: round-robin over healthy replicas; passive health (5xx/connect errors) +
  active `/health` probe; no retry after first byte was streamed.
- **Limits**: per-user rpm/tpm (token bucket in Postgres or in-memory per instance for v1), per-model concurrency.
- **Body size limit** (e.g. 32 MB) and model-name allow-list before forwarding.
- **Privacy**: never log prompts/responses; log only metadata.

## 10. Deployment

- Image: `python:3.12-slim`, FastAPI + uvicorn + httpx (HTTP/2 off to vLLM), Authlib/python-jose.
- Kubernetes on `kube.aip.de`: Deployment (2 replicas), Service, Ingress `llm.aip.de` (TLS), Postgres
  (existing cluster service or small StatefulSet), Secret for Keycloak client secrets + DB DSN.
- Helm chart in `deploy/helm/`. CI: lint, tests, image build, (later) deploy.

## 11. Milestones & acceptance tests

| # | Milestone | Done when |
|---|---|---|
| M0 | Repo, spec (this file), Keycloak realm + clients agreed | clients exist in AIP realm |
| M1 | Passthrough router, static key, both protocols | OpenAI + Anthropic SDK tests: text, stream, tools pass against aip-best; client disconnect cancels vLLM request |
| M2 | Keycloak JWT + API keys + groups | invalid/expired/revoked → 401; forbidden model → 403; `/v1/models` filtered |
| M3 | Portal (login, create/revoke key, usage, snippets) | user logs in via AIP SSO, creates key, uses it from Claude Code + opencode |
| M4 | Usage, limits, metrics | token counts match vLLM usage (±0); 429 on rpm/tpm; Prometheus scrape OK |
| M5 | Helm + kube deploy, docs, runbook | `llm.aip.de` live; load test (e.g. 50 concurrent streams) without errors; rollback documented |
| M6 | Pilot with a small group, then decide on Bifrost retirement | 2 weeks pilot feedback |

Rough effort: M1–M4 ≈ 1 week, M5–M6 + hardening ≈ 1 week.

## 12. Risks

- Streaming edge cases with specific clients (Claude Code, opencode) → test each in M1/M3.
- Claude Code may send Anthropic-only features (prompt caching headers, beta flags) → forward, ignore unknown; test.
- Keycloak realm/group naming unknown → M0.
- Single maintainer → keep code small, tests first, documented runbook.

## 13. Open questions (Monday)

1. Which Keycloak **realm** do AIP users log in with; who creates the clients?
2. Hostname (`llm.aip.de`?) and ingress/TLS on kube.aip.de; namespace and who operates it.
3. Postgres: existing service or new?
4. Which groups get which models and limits?
5. Which models besides `aip-best` (embeddings, smaller/faster models)?
6. Keep Bifrost in parallel during the pilot? (recommended: yes)
7. Implementation language: Python/FastAPI (recommended, fits team) or Go.
