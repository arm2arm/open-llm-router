# open-llm-router — Delivery plan for the first release

Cut from [PLAN.md](PLAN.md) on 2026-11-10. PLAN.md stays the requirements document; this file says
what is built first and with what. Live status: [issue #1](https://github.com/arm2arm/open-llm-router/issues/1)
and the [milestones](https://github.com/arm2arm/open-llm-router/milestones).

## Goal

- **Tue 17 Nov — the router works for API clients.** OpenAI SDK, Anthropic SDK and Claude Code
  against real vLLM replicas: streamed, balanced by load, authenticated by API key, restricted by
  group, limited per user, and every call recorded with tokens, time to first token and tokens
  per second.
- **Wed 18 Nov — self-service end to end.** A user logs in through Keycloak, creates a key in the
  portal, uses it and sees their usage. `docker compose up` starts everything.
- **Fri 20 Nov — pilot-ready.** Metrics and tracing, load test, security review, privacy
  controls, runbook.

About 59 hours of estimated work: seven and a half working days from Wed 11 Nov for one developer
working serially. With a second developer or agent on the parallel work below, the portal lands on
Tue 17 Nov and the release is pilot-ready on Thu 19 Nov. The deferred scope is about another
6–7 days.

## Stack

- **Backend:** Go, standard library `net/http` (server, client, reverse proxy); one static binary `olr`
- **Database:** PostgreSQL 17 with pgx and plain SQL (no ORM)
- **Identity:** Keycloak (OIDC) with go-oidc and x/oauth2; API keys hashed with SHA-256
- **Portal UI:** Vite, React, TypeScript, Tailwind CSS, shadcn/ui, TanStack Query; built to static
  files and embedded in the binary
- **Observability:** Prometheus client_golang, OpenTelemetry over OTLP (metadata only), `log/slog`
- **Tests:** `go test -race` with httptest, a Go mock backend, Playwright for the portal, the
  official `openai` and `anthropic` Python SDKs as compatibility clients
- **Packaging and CI:** multi-stage Docker build to a distroless image, Docker Compose, GitHub Actions
- **Model backends (external):** vLLM

Go rather than Rust: the standard library already has the streaming proxy, connection pooling and
cancellation this router needs, one process uses all cores, and one maintainer can read all of it.
The proxy is I/O-bound, so Rust's extra speed would not show next to GPU time.

## Performance targets

Checked by the load test (#18):

- the router adds less than 5 ms to the time to first token (p95)
- tokens per second through the router are within 2 % of a direct connection at 256 concurrent streams

## Schedule

| Days | Milestone | Issues | Effort | Demo at the end |
|---|---|---|---|---|
| Wed 11 Nov | M0 · Dev stack | #2 #3 #4 | 6.5 h | `docker compose up`, `make test`, CI green |
| Thu 12 – Fri 13 | M1 · Passthrough + balancing | #5 #6 #7 #8 | 12.5 h | SDKs and Claude Code through the router to mock and real vLLM; slow replica gets less load; disconnect stops generation |
| Fri 13 – Mon 16 | M2 · API keys + policy | #9 #10 #11 | 8 h | CLI-issued keys; 401/403; `/v1/models` filtered per caller |
| Mon 16 – Tue 17 | M3 · Usage + limits | #12 #13 | 8.5 h | token counts equal the backend's; timing per call; 429 at each limit; `/v1/me/usage` |
| Tue 17 – Wed 18 | M4 · Portal | #14 #15 #16 | 11.5 h | Keycloak login → create key in the portal → use it → see the usage |
| Thu 19 – Fri 20 | M5 · Pilot hardening | #17 #18 #32 #19 #20 | 12 h | 256 streams within the targets; a trace per call; a user deletes their own activity; review closed; runbook |
| later | M6 · Production pilot | #21 #22 | — | pilot users active; IdP switch was config-only |

Independent of the main line (second developer or agent, any time): #3, #14, #16 (against the API
shapes written down in #15), the load-test program in #18, and the lead-time items in #21.

## Scope cut

| PLAN | First release | Deferred (issue) |
|---|---|---|
| §5 API | chat/completions, completions, embeddings, messages, count_tokens, models, `/v1/me/usage`, health | — |
| §6 Identity | API keys (Bearer and `x-api-key`); portal login via OIDC; groups → policy, refreshed at login; default key expiry | Keycloak tokens on the API, group sync job (#24); trusted forwarders (#23); brokered dev realm, required claim (#25) |
| §7 Limits | `max_concurrent`, `rpm`, `tokens_per_day`, `tokens_per_month`; 429 with reset time | `tpm`, per-user overrides, strict reservation, interactive/batch split (#27) |
| §8 Routing | weighted least-outstanding, `max_inflight`, active + passive health, one retry before first byte, aliases | queue when full, session affinity, priority classes (#26) |
| §10 Data | `users` keyed by `(issuer, sub)`, `api_keys`, `usage` (with timing), `usage_daily`, `sessions`; `schema.sql` at start | partitions, retention job, migration tool (#29); `quota_overrides` (#27) |
| §11 Behaviour | all of it | — |
| §12 Portal | React app: keys, usage (with time to first token and tokens per second), connect snippets | request list, 30-day chart, admin pages (#28) |
| §13 Observability and privacy | `/metrics`, OTLP spans per call, request logs; users delete their own activity, operator erasure, privacy notice template (#32) | Grafana dashboard, trace backend in Compose (#30); automatic retention job (#29) |
| §14 Packaging | one image (Go binary with embedded portal); Compose: router, Postgres, Keycloak (`olr-dev` realm), two mock backends; CI | `upstream-idp` realm (#25); Kubernetes (#22) |

## Decisions for this release

Several of these were lifted by the deferred work that followed, such as Keycloak tokens on
the API and the router queue; the current state is in
[PLAN.md §0](PLAN.md#0-changes).

- `/v1/*` accepts API keys only; Keycloak is used for portal login.
- One router instance; in-flight, rate and concurrency counters are in memory.
- A full pool answers 503 with `Retry-After`; no router-side queue (vLLM queues internally).
- Plain SQL with pgx; idempotent `schema.sql` embedded and applied at startup.
- The backend does the OIDC login and keeps the session; no token reaches the browser. The React
  app talks only to `/portal/api/*`.
- Tracing is metadata only: tokens, timing, model, replica. Prompts and responses are never recorded.
- A user is identified by issuer plus `sub`; the router never merges users by email (that is
  Keycloak account linking).
- Users can delete their own per-request records; daily totals stay for quotas and accounting.
- Standard library first in Go; a dependency only for YAML, Postgres, OIDC, metrics and tracing.
- Brokered (federated) login needs no router code, only Keycloak configuration.

## Checkpoints

- **Fri 13 Nov, midday — go/no-go (#8).** Real vLLM and real clients work through the router. Run
  the first smoke test against the real backend on Thu 12 Nov, as soon as #5 works. If Anthropic
  passthrough is not usable on the deployed backend: ship with `protocols: [openai]` for that
  backend, or take the fallback in PLAN §2.
- **Tue 17 Nov.** From here the router is usable with CLI-issued keys even if the portal slips.
- **Before any real user.** #19 (security review) is closed.

## If we fall behind, cut in this order

1. Accounting tests for `/v1/completions` and `/v1/embeddings` (the passthrough stays)
2. OpenTelemetry export (per-call records and Prometheus metrics stay)
3. Dropping the injected usage chunk (always send it; note it in `docs/CLIENTS.md`)
4. `rpm` (concurrency and daily quota stay)
5. Portal usage page (`GET /v1/me/usage` stays)
6. Monthly quota

Never cut: cancel on disconnect, hashed keys, the model allow-list, usage records, the security review.

## Review input

Planning review [#31](https://github.com/arm2arm/open-llm-router/issues/31): the Go backend, the
backend-first order (keys work before the portal exists), the user key of issuer plus `sub` (#9)
and deletion of one's own activity (#32) are taken up in this plan.

## Needed from maintainers

- By Thu 12 Nov: a reachable vLLM endpoint that serves `/v1/messages`, and its model name
  (private, passed via environment).
- Claude Code and opencode installed for #8.
- Start the lead-time items of #21 now: production realm client, hostname/TLS, firewall.
