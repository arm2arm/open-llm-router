# Reference

Behaviour of the router in detail. Operations are in [RUNBOOK.md](RUNBOOK.md), the portal's
JSON API in [PORTAL_API.md](PORTAL_API.md), metrics and traces in
[OBSERVABILITY.md](OBSERVABILITY.md), clients in [CLIENTS.md](CLIENTS.md).

## API keys and bootstrap administration

All `/v1/*` requests require `Authorization: Bearer <key>` or `x-api-key: <key>`, or, with
`oidc.audience` set, an identity-provider access token as the bearer (see the runbook).
Matching credentials in both headers are accepted; conflicting or repeated headers are rejected.
Missing, malformed, unknown, revoked or expired keys and disabled users receive a generic 401
in the caller's OpenAI or Anthropic error format. `/healthz` and `/readyz` remain public.
Database authentication failures return 503. Portal sessions never authenticate `/v1/*`.

Model access comes from the user's stored groups and the config's `groups` section. Group
names match exactly. Allowed models are the union of all matching groups; `models: ["*"]`
grants every configured model. A user with no matching configured group has no model access,
including when the config has no groups. Bootstrap users therefore need `user add -groups staff`
or another configured group. `GET /v1/models` returns only allowed models and their aliases;
callers with no access receive `{"object":"list","data":[]}`.

Model checks run after alias resolution and before upstream-name rewriting or replica selection.
A forbidden model returns 403 with `permission_error` in the endpoint's OpenAI or Anthropic
error format, including for streaming requests and `/v1/messages/count_tokens`.
Numeric policy limits merge independently: the largest value wins, and an omitted limit in
any matching group means unlimited for that field. Unknown groups do not affect the result.
These limits apply per user, across all keys, protocols, models and replicas.

The bootstrap commands use `OLR_DATABASE_URL` and work without a running HTTP server. Under
Compose, run them as `docker compose exec -T router olr admin ...`; on the host use `bin/olr admin ...`.
`user add` creates an issuer-scoped `local` identity and prints its UUID. Save that UUID for
subsequent operations; adding the same local username again is an error.

```sh
# After creating a user and key as shown above:
docker compose exec -T router olr admin key list -user "$user_id"
docker compose exec -T router olr admin key create -user "$user_id" -label short-job -lifetime 24h
docker compose exec -T router olr admin key revoke -id <key-uuid-from-list>
docker compose exec -T router olr admin user disable -id "$user_id"
```

Key creation prints the plaintext once on stdout. Keys contain 32 bytes from `crypto/rand`,
encoded as 43 base62 characters after `olr-`. The SHA-256 hash and a 12-character display
prefix are stored. With `OLR_API_KEY_ENCRYPTION_KEY`, newly issued keys also store
AES-256-GCM ciphertext for explicit Admin copy; older keys remain unrecoverable.
Listing returns metadata only. Request logs contain neither credentials nor
hashes. Keys expire after `auth.default_key_lifetime_days` (default 90; range 1–36500), unless
`key create -lifetime` supplies a positive Go duration. The CLI reads this setting from
`-config` / `OLR_CONFIG`, or uses 90 days without a config file. No command issues indefinite keys.

Successful authentication is cached for 60 seconds, bounded by the key's expiry. `last_used`
changes at most once per minute per key, including across processes. Revoke and disable publish
Postgres notifications and wait for all running router listeners to clear their caches before
returning, so the next request is rejected. If acknowledgement fails, the CLI reports that the
change committed and must be retried. Each router holds one dedicated Postgres listener connection;
if it disconnects, cached credentials are discarded and requests use SQL lookups until it reconnects.
Direct SQL edits are picked up within the cache TTL; use the admin commands for immediate invalidation.
`make smoke` and `make sdk` create temporary test users and disable them when they finish.

## API behavior

The router serves `POST /v1/chat/completions`, `/v1/completions`, `/v1/embeddings`,
`/v1/messages` and `/v1/messages/count_tokens`. Request bodies are limited to 32 MiB.
Unknown fields and client protocol headers pass through. Client credentials and cookies are
removed; replicas can use their own bearer token via `bearer_token_env` in the config.
Backend URLs must be HTTP(S) origins without credentials, a path prefix or a query.

An alias resolves to its configured model; `upstream_model` optionally changes the name sent
to the backend. Only a rewritten request is serialized again, preserving unknown JSON values.
Responses, including backend errors and SSE bytes, pass through unchanged except for the
injected usage-only event described below. Redirects are
returned to the caller. Unsupported model/protocol combinations give a protocol-shaped 404.

Pools choose the healthy replica with the smallest `inflight / weight` and randomize ties.
Slots last until completion or disconnect. Full or unhealthy pools return 503 with `Retry-After: 1`,
or, with `queue_timeout_s`, after waiting that long in a priority-ordered queue (runbook). `/readyz` needs a healthy replica for every configured model and a successful
database ping, regardless of whether replica slots are currently full. The database check is
bounded to 2 seconds and returns 503 on failure; `/healthz` remains a process liveness check.
Health probes use each replica's `health_path` (default
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
curl http://localhost:8080/v1/models -H "Authorization: Bearer $OLR_API_KEY"
curl -N http://localhost:8080/v1/messages \
  -H "x-api-key: $OLR_API_KEY" \
  -H 'Content-Type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":true}'
```

The local stack uses deterministic mock answers. Visit `/portal/` to create a key,
copy a working client snippet and see your own usage without bootstrap administration.

For real Ollama, vLLM or SGLang backends, see [CLIENTS.md](CLIENTS.md) and
[config.real.example.yaml](../config.real.example.yaml). The runbook includes the recorded laptop
results, Claude Code/OpenCode tool sessions, backend cancellation evidence and `make sdk-real`
/ `make bench-real`. Against local vLLM 0.33.0, the SDK, cancellation and timing checks pass
and Claude Code and OpenCode complete a tool session with Qwen3 8B. The same checks on the
deployed GPU backend remain pending.

## Keycloak and portal login

Open [the portal](http://localhost:8080/portal/) after `make up`. Compose pins
Keycloak **26.8.2** and imports `deploy/keycloak/olr-dev-realm.json` on an empty data volume.
The confidential `olr-portal` client requires authorization code flow with PKCE S256;
implicit/password grants and service accounts are disabled. Groups appear as names in the
ID token. The import creates these public, development-only credentials:

| Realm | Username | Password | Group |
|---|---|---|---|
| `olr-dev` | `staff` | `staff-dev-only` | `staff` |
| `olr-dev` | `student` | `student-dev-only` | `students` |
| `olr-dev` | `admin` | `admin-dev-only` | `admins` |
| `master` (admin console) | `admin` | `keycloak-admin-dev-only` | Keycloak administrator |

The dev client secret is `olr-portal-dev-only`, in both the realm file and `.env.example`.
These values are for the local stack. Register a separate confidential client and use
deployment secrets for another environment. Imports preserve existing realms; changing the
realm file or bootstrap admin settings does not overwrite a populated Keycloak volume.
For a fresh local import, `docker compose down --volumes` deletes both local databases.

The browser issuer is `http://localhost:8081/realms/olr-dev`. Keycloak fixes `KC_HOSTNAME`
to that public origin and enables `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true`. The router sets
the same `OIDC_ISSUER`, and transports server-side requests via
`OIDC_BACKCHANNEL_URL=http://keycloak:8080/realms/olr-dev`. Discovery and the verified ID
token must still advertise the public issuer exactly, following the
[Keycloak hostname configuration](https://www.keycloak.org/server/hostname). No issuer or signature check is
disabled. The router discovers authorization, token, JWKS and logout endpoints; the
internal discovery runs independently of listening, with retries during outages.
`/portal/api/auth` reports SSO availability. `make portal-check` also checks
public discovery and completes real logins against the container to verify both sides.

`OLR_PORT` and `KEYCLOAK_PORT` change the default public origins in Compose. The realm's
redirects use its `PORTAL_BASE_URL` import placeholder, defaulting to
`http://localhost:8080/portal/callback` and `http://localhost:8080/portal/` after logout.
When copying `.env.example`, keep its explicit origins in sync with any changed ports.
Use `localhost` in the browser, matching the registered origin and cookies.

Host-side development against this Keycloak can use:

```sh
export OIDC_ISSUER=http://localhost:8081/realms/olr-dev
export OIDC_PORTAL_CLIENT_ID=olr-portal
export OIDC_PORTAL_CLIENT_SECRET=olr-portal-dev-only
export PORTAL_BASE_URL=http://localhost:8080
export PORTAL_COOKIE_SECURE=false
unset OIDC_BACKCHANNEL_URL  # the public issuer is reachable from the host
```

In deployment, set an HTTPS `PORTAL_BASE_URL`; `PORTAL_COOKIE_SECURE` defaults to true and
login refuses an HTTP public origin with secure cookies. Local HTTP development must
explicitly disable secure cookies. Local session APIs work without Keycloak; browser
forms are tracked in #74. See [the admin guide](ADMIN_GUIDE.md#use-local-authentication-without-keycloak).
`OIDC_GROUPS_CLAIM` and `OIDC_USERNAME_CLAIM`
override the default `groups` and `preferred_username` claims. All five OIDC settings can
override the YAML or enable OIDC with no YAML section. Use the public issuer directly when
reachable, or configure an internal backchannel without changing the expected issuer.
No provider-specific endpoints are hardcoded.

The backend stores only hashed opaque session IDs (eight hours) and one-use login
challenges (ten minutes) in Postgres. Browser cookies are HttpOnly and SameSite=Lax, scoped
to `/portal`. OIDC tokens are discarded after validation. Users match `(issuer, sub)`;
claims refresh on login, disabled users are refused, and API key caches see updated groups
before the login finishes. Sessions check the stored enabled flag on every request.

The portal's JSON API is described in [PORTAL_API.md](PORTAL_API.md).
It covers current user/limits, owned keys, per-model daily/monthly usage and allowed models.
Mutations require JSON and the configured origin; API keys are returned at creation
and through the explicit Admin copy route when encrypted recovery is configured.
Logout invalidates the local session and returns a discovered Keycloak logout URL for the
browser to visit. Keycloak asks for confirmation because no ID token is retained or sent
to the browser as a logout hint. `make portal-check` verifies the resulting SSO session is
ended and restores its temporary Keycloak group changes.

## Usage accounting

Each authenticated inference call creates one `usage` row, including rejected requests,
backend errors, and aborted responses. Retries remain one call, attributed to the final
selected replica. `/v1/models`, `/v1/messages/count_tokens`, probes, and authentication
failures are excluded. Rows contain the user/key IDs (with `forwarded_for` for calls a trusted forwarder made for a user), canonical model (empty for an unknown
or invalid model), replica origin, protocol, status, backend token counts, start time,
`latency_ms`, `ttfb_ms`, and `is_stream`. No prompt, completion, raw event, or credential is persisted or
logged. `ttfb_ms` measures the first backend body byte and is null when none arrives.
For streaming calls, derive output tokens/second as
`output_tokens * 1000 / (latency_ms - ttfb_ms)` when the denominator is positive;
this rate is not meaningful for non-stream responses that arrive all at once.

OpenAI streams request `stream_options.include_usage: true`. If the caller omitted it or
set it to false, the router removes only the extra event with empty `choices` and numeric
usage; all other events keep their original bytes. If the caller requested usage, that
event is delivered unchanged. Unknown request fields and stream options remain intact.
Anthropic input comes from `message_start`; output comes from the last `message_delta`.
An incomplete stream or client disconnect has status `aborted`; an explicit SSE error has
status `error`. Without final output usage, the number of observed content events is a
lower-bound estimate. Missing input usage is zero. Non-stream JSON is inspected incrementally
without retaining content. Inspection buffers at most 1 MiB per SSE event and 64 KiB per
usage object; oversized events pass through unchanged without inspection or filtering.

Four writer goroutines drain a 1,024-record queue. Each insert and its `usage_daily` upsert
commit in one transaction, using the request's start date in `limits.timezone` (default UTC).
Database writes have a
2-second deadline. A full queue drops metadata instead of delaying inference; failed and
dropped writes are logged generically and counted by `usage.Writer.Counters()`. The HTTP
shutdown grace runs first, followed by up to 5 seconds to flush accounting before closing
the database. A database failure during that flush is logged without changing client
responses. The additive startup schema upgrade marks legacy rows `is_stream=false`;
their TTFB remains available, but their generation rates are excluded from averages.

## Personal request content

Separate from metadata accounting, `internal/requesttrace` provides default-enabled,
user-controlled content capture and a completed-request trace viewer. Retention defaults
to 7 days and each user can choose 1–30 days. Two background workers decode and store
bounded content independently of usage accounting. See [REQUEST_TRACING.md](REQUEST_TRACING.md)
for exact limits, privacy, deletion and failure behavior. Existing metadata-only history
cannot be backfilled.

## Limits and current-user usage

After authentication and model authorization, inference routes acquire a per-user
concurrency slot for the key's class (interactive or batch), check a sliding 60-second RPM
window, the TPM token bucket, and remaining daily and monthly token quota, in that order. A slot lasts through the full response or stream and
is released on completion, cancellation, failure, or disconnect. A request passing the
RPM gate consumes an attempt even if the quota gate or backend later rejects it; a
concurrency rejection consumes no RPM. `/v1/messages/count_tokens` uses these gates but
does not charge tokens. `/v1/models` and `/v1/me/usage` consume no limits.

A rejected gate returns HTTP 429 with `Retry-After` seconds and the endpoint's native
JSON error envelope. `error.type` is `rate_limit_error`; `error.limit` identifies
`max_concurrent`, `max_concurrent_batch`, `rpm`, `tpm`, `tokens_per_day`, or `tokens_per_month`, and `error.reset_at`
is an RFC3339 timestamp, also named in the message. Concurrency uses an estimated one-second
retry: a stream may release a slot sooner or remain active longer. RPM retries at the
oldest accepted attempt plus 60 seconds. Day/month quotas reset at local midnight in the
configured IANA timezone, including daylight-saving changes:

```yaml
limits:
  timezone: Europe/Berlin  # default UTC; same calendar as usage_daily
```

Quota counts **input plus output tokens** from completed/aborted calls, charged to the
request's start date. The charge updates memory synchronously before releasing the slot,
then queues the database write. Already admitted calls can overshoot a quota unless the
group sets `strict_quota`, which reserves `max_tokens` plus the request size before each call.
Without it, overshoot is bounded by the total tokens of admitted calls. `max_concurrent × max_tokens` bounds only their output,
not prompt tokens; without finite concurrency and bounded requests there is no fixed bound.

Counters are in memory for one router instance. The first request or usage lookup for a
user loads the current month's `usage_daily` totals and today's timing metadata from one
database snapshot; later calls use the cache, including records still queued for storage.
Unavailable initial counters return a generic 503 with a one-second retry. Token quotas
survive restarts through committed daily totals; RPM and in-flight counts restart empty.
Failed/dropped writes remain charged in memory but cannot be recovered after a crash or
restart. Multiple active router instances and out-of-band usage writes are unsupported
in v1. Changing the timezone on a populated database requires rebucketing `usage_daily`
from `usage` while the router is stopped.

```sh
curl --fail http://localhost:8080/v1/me/usage \
  -H "Authorization: Bearer $OLR_API_KEY"
```

`GET /v1/me/usage` returns only the authenticated user's snapshot. Its fields are:

| Field | Meaning |
|---|---|
| `timezone` | Configured quota calendar |
| `limits` | Effective `tokens_per_day`, `tokens_per_month`, `rpm`, `tpm`, `max_concurrent`, `max_concurrent_batch` (null means unlimited) and `strict_quota`, after per-user overrides |
| `used` | `tokens_today`, `tokens_this_month`, and attempts in the `rpm` window |
| `remaining` | Corresponding token, RPM and TPM headroom, clamped to zero; null for unlimited fields |
| `reset_at` | Next `day`/`month` boundary and oldest RPM attempt's expiry (`rpm`, null when empty/unlimited) |
| `in_flight`, `in_flight_batch` | Currently reserved user slots, and those of batch keys |
| `reserved_tokens` | Strict-quota reservations of calls in flight |
| `models` | Today's per-model `input_tokens`, `output_tokens`, `average_ttfb_ms`, `average_tokens_per_second`, sorted by `model`; empty array without usage |

Model averages are arithmetic means of measured requests. TTFB excludes missing values;
tokens/second includes only streams with positive output and generation duration. Missing
measurements are null, and non-stream responses never contribute to throughput averages.
`used.rpm` is null for unlimited RPM because those attempts are not retained.

The splitter has every-byte boundary tests, a no-allocation content-event check, and a
bounded fuzz run in CI. To run a longer local fuzz check:

```sh
go test ./internal/usage -run '^$' -fuzz '^FuzzStreamPreservesBytes$' -fuzztime=30s -parallel=2
```

## Running outside Compose

For a host-side router, run Postgres 17 locally (or `docker compose up -d postgres`) and export
`OLR_DATABASE_URL='postgres://olr:olr-dev-only@127.0.0.1:5432/olr?sslmode=disable'` with the
appropriate credentials. The URL is required: startup fails before listening if connecting
or applying the schema fails. The numbered migrations in [internal/db/migrations](../internal/db/migrations)
are applied at startup in one transaction and recorded in `schema_migrations`; a router refuses
a database with a newer schema version. `olr migrate` applies them without serving.
To change the schema, add the next `NNNN_name.sql`; never edit an applied migration.

Without Docker, build both binaries, run two mock processes on ports 8001/8002, and copy the
example config with pool URLs changed to `http://127.0.0.1:8001` and `http://127.0.0.1:8002`.
Remove the `oidc` section to use API keys without portal login. To enable the portal, export
the OIDC and portal settings below before running `bin/olr serve -config <local-config>`.

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
[config.example.yaml](../config.example.yaml) explains the format. It refuses to start on an unknown
key, on a `${VAR}` that is not set, or on a value that fails validation, and lists every problem
it found. Secrets are read from the environment only (`OLR_DATABASE_URL`,
`OIDC_PORTAL_CLIENT_SECRET`, `OIDC_SYNC_CLIENT_SECRET`, `OLR_METRICS_TOKEN`, backend tokens). Logs are JSON lines on stdout with a request ID, and never contain
request or response bodies.

| Path | Content |
|---|---|
| `cmd/olr` | the binary; `olr serve` runs the router |
| `cmd/mock-llm`, `internal/mockllm` | deterministic OpenAI- and Anthropic-compatible test backend |
| `internal/config` | config file: `${VAR}` references, strict decoding, validation |
| `internal/db` | pgx connection pool and versioned migrations (`internal/db/migrations`) |
| `internal/retention` | nightly deletion of usage older than the retention periods |
| `internal/oidcclient` | identity-provider discovery and backchannel transport |
| `internal/apitoken` | identity-provider access tokens on `/v1/*` |
| `internal/groupsync` | Keycloak group and enabled-state sync |
| `internal/auth` | API key generation, authentication cache, invalidation and bootstrap administration |
| `internal/policy` | group policy resolution: allowed models and effective numeric limits |
| `internal/usage` | incremental usage inspection, timing metadata, asynchronous atomic accounting |
| `internal/limits` | per-user concurrency, sliding-minute RPM, calendar token quotas and usage snapshots |
| `internal/portal` | OIDC code/PKCE login, opaque sessions and scoped JSON APIs |
| `internal/home` | public project overview and portal sign-in entry point at `/` |
| `web` | Vite/React/TypeScript portal, Tailwind and shadcn/ui, TanStack Query, embedded build assets |
| `internal/metrics` | protected Prometheus metrics and opt-in OTLP/HTTP call spans |
| `internal/server` | HTTP server: routes, request ID and request log, `/healthz`, `/readyz`, graceful shutdown |
| `internal/proxy`, `internal/apierr` | bounded request bodies, protocol errors, byte-preserving streaming and model listing |
| `internal/pool` | weighted selection, in-flight caps, queue, session affinity, health checks and cooldown |
| `internal/reqid` | the request ID carried in a request's context |
| `deploy/compose` | the local development stack, load test and optional observability stack |
| `deploy/keycloak` | dev realms `olr-dev` and `upstream-idp` |
| `deploy/observability` | Grafana dashboard, Prometheus and Grafana provisioning |


## Tests

Unit tests live next to their code. HTTP streaming and disconnect tests use
`httptest.NewServer` and real sockets, without requiring a running Compose stack.
Postgres integration tests in `tests/integration` skip when `OLR_TEST_DATABASE_URL` is unset.
Each test creates and drops its own database, leaving the supplied database intact. The test
role needs `CREATEDB` and permission to terminate its own connections (the Compose and CI
roles have these permissions). Tests cover schema initialization/restarts, issuer-scoped
identities, deletion rules, database loss/recovery, and CLI-created keys through the router binary,
including immediate cache invalidation across two router processes on revoke and user disable,
plus group-based model access, filtered model lists, usage persistence, privacy, atomic daily
totals under 100 concurrent calls, shutdown flushing, shared user limits, stream slot
release, calendar resets, quota recovery after restart, and the usage API for real API keys.
Portal tests use a signing OIDC provider and real Postgres to test state, nonce, issuer,
audience, expiry, replay prevention, session rotation, disabled users, origin checks, custom
claims, scoped usage/keys, group refresh and immediate API-key cache invalidation.
With Compose running on the default database port, include them with:

```sh
OLR_TEST_DATABASE_URL='postgres://olr:olr-dev-only@127.0.0.1:5432/olr?sslmode=disable' make test
```

CI runs on pushes and pull requests. It uses the same `make lint`, `make vuln` and `make test`
commands with a Postgres 17 service, while a parallel job builds both runtime images, starts
Compose and runs the frontend checks, Playwright self-service journey, `make smoke`,
`make portal-check`, and official SDK checks against the router and both mocks. The Go
job has a five-minute timeout and the image/browser job ten minutes. Dependencies are cached.

