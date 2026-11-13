# Portal backend API

The portal is served at the configured `PORTAL_BASE_URL` origin. `/portal/*` uses an
opaque eight-hour session cookie; `/v1/*` continues to require API keys and ignores cookies.
The initial local password-change session is restricted and lasts ten minutes.
All portal responses use `Cache-Control: no-store`. Errors are JSON:
`{"error":{"type":"authentication_error","message":"authentication required"}}`.
Unauthenticated API calls return 401; forbidden origins/disabled logins return 403;
invalid input returns 400, unknown or other users' keys return 404, and unavailable
dependencies return 503. No OIDC token is stored or returned to the browser.

## Login and logout

- `GET /portal/api/auth`: public authentication metadata,
  with `default` (`oidc` or `local`: the primary sign-in option),
  `local: {available, login_url, password_url}` and
  `oidc: {configured, available, login_url}`. No secret or account-existence information
  is returned. The OIDC `login_url` points to the browser entry below.
- `GET /portal/sign-in`: browser sign-in page, with Keycloak primary when configured
  and a visible local form. Unavailable Keycloak is reported separately.
- `GET /portal/login/local`: redirects locally to `/portal/sign-in/local`, a direct
  browser local form that never forces SSO.
- `GET /portal/sign-in/keycloak`: browser SSO entry; starts the existing OIDC flow
  when available and returns to `/portal/sign-in?sso=unavailable` otherwise.
  `GET /portal/login` retains its direct OIDC/JSON error behavior for API clients.
- `POST /portal/login/local`: accepts `{"username":"admin","password":"…"}` from the
  configured Origin and Host. Returns `{"must_change_password":true|false}` and a new
  server-side session cookie, revoking the browser's old session and pending SSO challenge.
  Unknown, disabled, SSO-only users and wrong passwords return the same 401 error.
  Local identity is keyed by the exact local username, never linked to an SSO identity.
- `GET /portal/api/auth/session`: returns the session's `auth_method` (`local` or `oidc`)
  and `must_change_password`, including for restricted sessions. A required-change session
  gets 403 `password_change_required` on all other authenticated APIs except password
  change and logout. No inference or Admin access is granted before the first change.
- `POST /portal/password`: requires a local session and accepts
  `{"current_password":"…","new_password":"…"}`. A new password must differ, contain
  at least 15 UTF-8 characters and fit within 1024 bytes. Success revokes **all** sessions,
  clears the cookie and returns `{"login_required":true}`. Sign in again with the new
  password. Policy errors return 400; an incorrect current password returns generic 401.
  Each local session is bound to the credential revision verified at login, preventing
  an older verification from establishing a usable session after a password change.
- `GET /portal/login`: starts authorization code login with fresh state, nonce and PKCE S256.
  A ten-minute HttpOnly cookie binds the one-use server-side challenge to this browser.
  At most 10,000 unfinished challenges exist at a time; beyond that the route answers 503
  with `Retry-After: 60`. Without OIDC it redirects to `/portal/login/local`.
  Unavailable configured SSO returns 503 `oidc_unavailable` with `Retry-After: 10`.
- `GET /portal/callback`: consumes the challenge, exchanges the code and validates the ID
  token's signature, issuer, audience, expiry and nonce. The user is upserted by `(issuer, sub)`;
  email is never an identity key. Username, email and groups refresh on each login. Disabled
  users cannot log in. A successful login rotates the session and redirects to `/portal/`.
- `POST /portal/logout` with `{}`: deletes this session and clears its cookie. Returns
  `{"logout_url":"<discovered end_session_endpoint with client_id and post_logout_redirect_uri>"}`.
  A local session returns `{"logout_url":"/"}` without contacting the
  provider. SSO sessions use the provider URL when available and fall back to the local
  homepage URL during an outage. The UI navigates to that URL. The provider's
  `post_logout_redirect_uri` is the configured portal origin followed by `/`, so
  both login methods return to the public homepage. Keycloak asks for
  confirmation because no ID token is retained or exposed as an `id_token_hint`.

Session cookies are `HttpOnly; SameSite=Lax; Path=/portal`, with `Secure` on by default.
Only local HTTP development explicitly sets `PORTAL_COOKIE_SECURE=false`.
State-changing requests, including logout and DELETE, require `Content-Type: application/json`
and an `Origin` exactly matching `PORTAL_BASE_URL`; cross-site Fetch Metadata is rejected.
The UI can use `fetch(url, {method, headers: {'Content-Type': 'application/json'}, body: ...})`.

Local login and password change use bounded 8 KiB JSON bodies and fixed one-minute
throttle windows: ten attempts per username, twenty per socket source address and sixty
globally. All attempts count. `429` includes `Retry-After`; rejections never extend a
window or disable an account. Forwarding headers are ignored. Hashed buckets persist in
PostgreSQL, with a 4096-row bound, and at most two password operations run concurrently
per process. See [the guide](ADMIN_GUIDE.md#use-local-authentication-without-keycloak)
for a cookie-jar workflow that keeps passwords out of command arguments.

## Current user

`GET /portal/api/me`:

```json
{
  "user": {"id":"uuid","username":"staff","email":"staff@example.test","groups":["staff"],"protected_admin":false},
  "limits": {"tokens_per_day":2000000,"tokens_per_month":null,"rpm":60,"tpm":null,
             "max_concurrent":4,"max_concurrent_batch":8,"strict_quota":false}
}
```

Limits resolve from the current stored groups, then the user's overrides (`olr admin
override`); null means unlimited. `max_concurrent` applies to interactive keys and
`max_concurrent_batch` to batch keys; with `strict_quota` every call reserves its
`max_tokens` plus its request size until it ends. An empty group list
is an empty JSON array and grants no models unless a matching configured group exists.

## API keys

- `GET /portal/api/keys`: `{"keys":[{"id":"uuid","prefix":"olr-...","label":"laptop",
  "created":"RFC3339","last_used":null,"expires":"RFC3339","revoked":false}]}`.
  Only the session user's keys are listed. Neither hashes nor full keys are returned.
- `POST /portal/api/keys`: accepts `{"label":"laptop","expires_in_days":7,"class":"batch"}` (label: 1–100 bytes after trimming).
  `class` is `interactive` (default) or `batch`; the list returns it for every key.
  `expires_in_days` is optional, an integer from 1 to `auth.max_key_lifetime_days` (default 90). Omission uses
  `auth.default_key_lifetime_days`. Returns 201 `{"key":"olr-..."}` exactly once.
  The site active-key cap returns 409; an over-limit lifetime returns 400.
  Refresh the key list for metadata after creating a key. Unknown fields are rejected.
- `DELETE /portal/api/keys/{uuid}` with `{}`: revokes an owned active key, returning 204 with
  no body. On a key that is already revoked or expired it removes the key for good (also 204);
  usage records that referenced it remain without the key reference, and a further DELETE
  returns 404. Other users' and nonexistent keys return the same 404. Revocation clears API
  authentication caches before success, so the next API-key request is rejected. Existing
  sessions do not authorize `/v1/*` requests.

## Usage

`GET /portal/api/usage`:

```json
{
  "quota": {
    "timezone":"UTC",
    "limits":{"tokens_per_day":2000000,"tokens_per_month":null,"rpm":60,"max_concurrent":4},
    "used":{"tokens_today":10,"tokens_this_month":10,"rpm":1},
    "remaining":{"tokens_today":1999990,"tokens_this_month":null,"rpm":59},
    "reset_at":{"day":"2026-11-12T00:00:00Z","month":"2026-12-01T00:00:00Z","rpm":"2026-11-11T12:01:00Z"},
    "in_flight":0,
    "models":[{"model":"chat-model","input_tokens":3,"output_tokens":7,"average_ttfb_ms":50,"average_tokens_per_second":20}]
  },
  "today":[{"model":"chat-model","requests":1,"input_tokens":3,"output_tokens":7,"average_ttfb_ms":50,"average_tokens_per_second":20}],
  "month":[{"model":"chat-model","requests":1,"input_tokens":3,"output_tokens":7,"average_ttfb_ms":50,"average_tokens_per_second":20}]
}
```

`quota.limits` has the fields of `/portal/api/me`; `remaining.tpm` is the token bucket's
current level, `in_flight_batch` the calls of batch keys within `in_flight`, and
`reserved_tokens` the strict-quota reservations of calls in flight.
`quota` has the same contract as `/v1/me/usage`, including live in-memory charges. `today`
and `month` are sorted per-model database aggregates, empty arrays without usage, and may
lag live charges while the asynchronous writer drains. Requests include recorded failures.
Calendar boundaries use `limits.timezone`. TTFB averages exclude missing measurements;
throughput averages include only measured streams with positive output/duration. Missing
averages are null. All queries are scoped to the session user.

## Deleting activity

`DELETE /portal/api/usage` with `{}` deletes every per-request record attributed to the session
user, including calls a trusted forwarder made for them, and returns `{"deleted":<rows>}`.
Daily totals (`usage_daily`) stay, so `quota` and the `today`/`month` token totals are
unchanged; timing averages of the deleted records disappear. Other users' records are
untouched. See [PRIVACY.md](PRIVACY.md).

## Requests and daily totals

- `GET /portal/api/requests`: `{"limit":200,"requests":[{"time":"RFC3339","model":"chat-model",
  "protocol":"openai","status":"200","input_tokens":3,"output_tokens":7,"latency_ms":400,
  "ttfb_ms":50,"stream":true,"key_prefix":"olr-…","forwarded":false,
  "request_id":"32-hex-characters","operation":"chat/completions","trace_available":true,
  "trace_summary":{"input_messages":3,"output_messages":1,"tool_calls":1,"tool_results":1,
  "tools":2,"finish_reason":"tool_calls","cached_tokens":0,"reasoning_tokens":0,
  "truncated":false},"conversation":"c…"}]}`, newest first, the
  user's last 200 records (metadata only). `key_prefix` is null once the key is removed, and
  for calls a trusted forwarder made for the user (`forwarded: true`).
- `GET /portal/api/usage/daily`: `{"timezone":"UTC","days":[{"day":"2026-10-13","requests":0,
  "input_tokens":0,"output_tokens":0}, …]}`: exactly 30 days ending today in `limits.timezone`,
  oldest first, from the daily totals (unaffected by deleting activity).

## Personal trace settings and content

These routes require a full session. Mutations require the configured Origin and JSON
content type. All access is scoped to the session user; admin status adds no content access.

- `GET /portal/api/trace-preferences`: `{"enabled":true,"retention_days":7}`.
- `PUT /portal/api/trace-preferences`: the same complete object; boolean `enabled` is
  required and integer `retention_days` must be 1–30. Returns the saved object.
- `GET /portal/api/requests/{request_id}`: `{"request_id":"…","expires_at":"RFC3339",
  "detail":{"operation":"chat/completions","input":[],"output":[],"events":[],
  "parameters":{},"truncated":false,"warnings":[]}}`. Inputs/outputs contain
  `{role,blocks:[{type,text?,name?,id?,arguments?}]}`. Types include `text`, `reasoning`,
  `tool_call`, `tool_result`, `refusal` and attachment markers. Events contain
  `{ms,type,text?}` with relative arrival time. Missing, expired and another user's IDs
  all return 404. Older metadata rows have `request_id:null`.
  Version 2 detail (`"v":2`) adds `tools` (`{name,description?,schema?}`), `request` and
  `response` (sanitized raw JSON), `frames` (`{ms,data}` raw stream payloads), `routing`
  (`requested_model`, `model`, `upstream_model`, `changes:[{field,action,from?,to?}]`,
  `attempts:[{replica,queue_ms,ms,status?,error?}]`, `admission_ms`, `headers` (names),
  `upstream_status`, `content_type`), `result` (`id`, `model`, `system_fingerprint`,
  `finish_reasons`, numeric `usage` flattened with dots) and `summary`. Messages carry a
  `hash`. Traces without `v` were stored by older routers.
- `GET /portal/api/requests/events`: `text/event-stream` of the caller's request
  lifecycle, metadata only. After `event: ready`, each `data:` line is a JSON object with
  `type` (`started`, `progress`, `finished`, `trace_ready`), `request_id` and, depending on
  the type, `time`, `model`, `operation`, `protocol`, `elapsed_ms`, `bytes`, `writes`,
  `stream`, `status`, `input_tokens`, `output_tokens`, `latency_ms`, `ttfb_ms`. Comments
  (`: ping`) arrive every 15 seconds; `event: end` (session ended or server stopping)
  closes the stream. More than 4 open streams per user and instance return 429.
- `DELETE /portal/api/request-traces` with `{}`: 204, permanently clears only the caller's
  content and invalidates pending writes. `DELETE /portal/api/usage` also clears content;
  daily quota totals remain.

Disabling invalidates pending captures while keeping historical content. Shorter
retention shortens existing expiry; longer retention applies only to new traces.
See [REQUEST_TRACING.md](REQUEST_TRACING.md) for bounds and supported field semantics.

## Admin

Members of a group with `admin: true` in the config and the protected local administrator
get `"admin": true` in `/portal/api/me` and these routes; everyone else gets 403.
Membership and protected status are read from the database on each request. Protected
authority does not grant inference model access; normal model policies still apply.
Local session APIs and browser sign-in/password forms work without Keycloak.
The browser gates workspace/Admin on completing a required first password change,
labels the identity source and offers local rotation under **Account**. API
authorization remains enforced on the server regardless of browser navigation.

- `GET /portal/api/admin/users?match=text`: up to 500 users whose username or email contains the
  text (case-insensitive), with groups, enabled flag, active key count, tokens today and this
  month, overrides by field, `issuer`, `protected_admin`, `has_local_password` and
  `must_change_password`.
- `GET /portal/api/admin/users/{id}`: the user, effective `limits`, `overrides` with `reason`,
  `set_by` and `set_at`, sorted allowed public `models` (including aliases), effective
  `admin` and key metadata. The user includes `issuer`, `has_local_password` and
  `must_change_password`; keys include `id`, `prefix`, `label`, `created`, `last_used`,
  `expires`, `revoked`, `expired`, `class`, `trusted_forwarder` and `copy_available`, never hashes, ciphertext or raw keys.
- `POST /portal/api/admin/users/{id}/keys/{key}/copy` is retired. It returns 410
  `key_recovery_removed` for an authorized session with valid Origin, 403 otherwise.
  No key can be recovered. `copy_available` is always false for compatibility.
  See [replacement and migration](ADMIN_GUIDE.md#replace-lost-api-keys).
- `POST /portal/api/admin/users` with
  `{"username":"alice","email":"alice@example.org","groups":["staff"],"password":"…"}`
  creates a new local identity and returns 201 `{"id":"UUID","must_change_password":true}`.
  Username is exact, 1–128 bytes, without whitespace/control characters. Email is optional
  and must be a valid address of at most 254 bytes. Password policy matches `/portal/password`.
  Group names must be distinct existing configured groups (maximum 64); omitted groups
  mean `[]`. Existing local usernames return 409; an SSO account with matching name/email
  remains separate. Unknown JSON fields and invalid input return 400.
- `PUT /portal/api/admin/users/{id}/groups` with `{"groups":["staff"]}` assigns configured
  memberships to an ordinary local user. The array is required; `[]` removes all memberships.
  Returns 204, deletes all browser sessions and synchronously invalidates API-key caches
  on every router. Existing keys use the updated access/limits on their next request.
  Existing per-user overrides are preserved.
- `POST /portal/api/admin/users/{id}/password` with `{"password":"…"}` sets a temporary
  password on an ordinary local user, including a CLI API-key-only account. Returns 204,
  deletes all sessions, permanently revokes all existing API keys and invalidates their
  caches. The user must change this password at next login. Reusing the current password
  returns 400. Creation/reset share global/IP login budgets, have a separate per-admin
  actor bucket (10/minute), and use the existing two-worker password-hashing cap; 429
  includes `Retry-After`.
- The group/password routes return 403 for Keycloak identities and the protected local
  administrator; their source/protection checks also run in the store transaction.
  Creating the reserved local `admin` returns 403. There is no portal deletion API.
- `DELETE /portal/api/admin/users/{id}/keys/{key}` with `{}` returns 204 after revoking a
  key belonging to that user. Unknown/mismatched owners return 404. Metadata and usage
  remain; browser sessions remain active. API-key caches invalidate on every router.
- `PUT /portal/api/admin/users/{id}/overrides/{field}` with `{"value":N,"reason":"…"}` (`null` for
  unlimited) sets an override, recorded as `set_by: "portal:<admin username>"`;
  `DELETE …/overrides/{field}` with `{}` clears it. Both return 204 and take effect at the user's
  next request on every router, including cached OIDC access-token principals.
- `PUT /portal/api/admin/users/{id}/enabled` with `{"enabled":false}` disables a user (sessions
  are deleted and API-key access stops working at once) or re-enables them; re-enabling
  requires a fresh browser login and restores only unrevoked, unexpired keys. Cached OIDC access-token
  principals are refused on their next request. Admins cannot disable themselves.
  Disabling the protected local administrator returns 403, including for another admin.
- `GET /portal/api/admin/groups`: per configured group, enabled members, requests and tokens today
  and tokens this month. A user in several groups counts in each.
- `GET /portal/api/admin/replicas`: per model, each replica's origin, health, in-flight calls,
  `max_inflight` and weight.

## Admin endpoint configuration

These routes require a full admin browser session. Mutation requests use the
normal JSON and same-origin checks (8 KiB body limit, unknown fields refused).
Resolved backend secrets are excluded from all responses and revisions.

- `GET /portal/api/admin/endpoints` returns `models`, `credential_references` and
  `allowed_private_networks`. Each entry contains `model`, `configuration`, current
  `revision` (initially `0`), `source` (`file` or `database`), `file_configuration`
  and the latest 20 `history` entries. History contains `id`, `created_at`, `file`
  and `configuration` (`null` denotes file ownership).
- `POST /portal/api/admin/endpoints/test` accepts `model` and `configuration`.
  It validates and probes without saving/publishing. Health results return 200
  with `{ok, message, replicas}`; `ok:false` means the probe failed. Validation
  failures return 400. A passing probe does not prove the upstream model exists.
- `POST /portal/api/admin/endpoints/models` accepts
  `{"model":"chat-model","replicas":[{"url":"http://ollama:11434","bearer_token_env":""}]}`.
  Returns `{models, complete, replicas:[{url, models, source?, error?}]}`. `models`
  is the sorted intersection across every replica, empty if any catalog failed;
  per-replica lists are diagnostic. Uses `GET /v1/models`, with `/api/tags` fallback
  only on 404/405/501. Destinations and credential references use the same policy
  as inference, without browser credentials, environment proxies or redirects.
  Authentication errors are not retried on the fallback path. Limits: 1–32 origins,
  1 MiB/2048 entries per catalog, 3 seconds per request, 8 seconds overall, at
  most 8 concurrent requests. Paginated catalogs require manual entry. Validation
  returns 400; backend failures return 200 with `complete:false` and safe errors.
  This operation saves no revision, performs no inference and changes no health
  or admission state. Catalog presence does not prove model capabilities.
- `PUT /portal/api/admin/endpoints` accepts `model`, the last observed `revision`
  and the complete `configuration`. It validates, probes every replica in that
  model, commits a revision and activates new requests. Returns 200 `{models}`.
- `POST /portal/api/admin/endpoints/restore` accepts `model`, the last observed
  `revision` and `target_revision`. Target `0` means current YAML/environment;
  other targets must belong to the model. Restore validates/probes and creates
  a new revision, returning 200 `{models}`.

Example configuration for an existing logical `chat-model`:

```json
{
  "model":"chat-model",
  "revision":0,
  "configuration":{
    "upstream_model":"qwen3.8:27b-mlx",
    "protocols":["openai"],
    "timeout_s":900,
    "idle_timeout_s":120,
    "queue_timeout_s":0,
    "session_affinity":false,
    "priority":{"interactive":0,"batch":0},
    "pool":[{
      "url":"http://192.168.178.81:11434",
      "health_path":"/",
      "weight":1,
      "max_inflight":1,
      "bearer_token_env":""
    }]
  }
}
```

Use GET's complete configuration as the edit base. A model requires 1–32 unique
replica origins. Credential names must be server-allowlisted; new private origins
require `OLR_BACKEND_ALLOWED_CIDRS`. URL credentials, base paths, queries and
fragments are refused. New loopback/link-local/metadata destinations are blocked;
file-defined destinations remain trusted. Health requests and inference apply the
same checked-IP dial policy and do not follow redirects. See the
[admin guide](ADMIN_GUIDE.md#destination-and-credential-setup) for operator setup.

Errors: 400 invalid draft or failed activation probe; 403 non-admin or invalid
Origin; 404 unknown logical model or revision from another model; 409 stale edit;
503 storage/service unavailable. Failed activation leaves the current revision.
Existing requests retain their routing snapshot; per-user admission and accounting
are retained. Activation supports one router process, without a broadcast to peers.
Logical names, aliases and group policies remain file-owned; these routes do not
create/delete logical models or edit permissions.

## Admin authentication settings

`GET` requires an admin session. Every other route also requires a **local**
administrator session established within the last 15 minutes; otherwise it
returns 403 `reauthentication_required`. SSO sessions cannot change how people
sign in. Bodies use the normal JSON and same-origin checks. Responses never
contain secret values: secrets appear as `configured`, `reference`
(`environment` or `stored`), `id`, `created_at` and `available`.

- `GET /portal/api/admin/settings/authentication` returns `settings` (the managed
  settings view: `managed`, `version`, `effective` values with `source`, `active`
  and `draft` revisions with documents, secret states, `validation` and `checks`,
  `conflicts` and `history`), `urls` (`redirect_uri`, `post_logout_redirect_uri`,
  `web_origin` to register on the portal client), `runtime` (the revision this
  process loaded, a startup `problem`, `sso_configured`, `sso_available`),
  `can_change`, `change_requirement` and `encryption_key_configured`.
- `POST …/import` with `{}` makes the database the authority; 409 if already managed.
- `PUT …/draft` with `{"version":N,"document":{…},"secrets":{"sync_client_secret":"…"}}`
  saves a draft based on the active revision. Empty secret values are ignored.
  400 lists field problems; 409 reports a stale version or file-managed settings.
- `POST …/discard` with `{"version":N}` drops the pending draft.
- `POST …/checks` with `{"revision":R,"test_client_id":"…","test_client_secret":"…"}`
  runs connection checks for revision `R` (`0`: the current configuration) and
  stores them on the revision. Returns `{revision, results, view}`; each result is
  `{name, status, message}` with `status` `passed`, `failed` or `skipped` for
  `discovery`, `logout_metadata`, `portal_credentials` (client authentication at
  the introspection endpoint), `service_token_audience` (needs the optional test
  service client, used once and not stored) and `sync_permissions` (lists one user
  through the Keycloak admin API). Messages are fixed text without secrets or
  provider responses.
- `POST …/test-sign-in` with `{"revision":R}` (a managed revision) returns
  `{"url":"…"}` for the browser. It sets a separate `olr_test_login` cookie; the
  provider returns to the normal `/portal/callback`, which verifies the identity
  token, the username and groups claims and the required claim, stores a
  `test_sign_in` result on the revision and redirects to
  `/portal/admin?authentication_test=passed|failed#authentication`. It creates no
  user or session, and only the administrator who started it, still freshly
  signed in locally, can complete it.

- `POST …/activate` with `{"version":N,"revision":R}` activates the current draft
  and applies it to this router at once. An SSO-enabled draft needs stored checks
  without failures and a passed test sign-in (400 otherwise), and its provider must
  be reachable now; on any refusal nothing changes. The portal login client,
  API-token verification and group sync switch together.
- `POST …/rollback` with `{"version":N,"revision":R}` re-activates a previously
  active revision (404 otherwise) and applies it, even while that provider is down.
- `POST …/apply` with `{}` applies the active revision to this router, for example
  after a CLI activation.
- The view also contains `other_issuers`: `{issuer, enabled_users, active_keys}`
  for SSO accounts of issuers other than the current one. On an issuer change,
  pending SSO logins are discarded and other issuers' SSO sessions stop
  authenticating; accounts are never merged across issuers.

Connection checks and test sign-in contact only the issuer (or its back-channel
URL), never follow redirects, and refuse link-local, metadata, multicast and
unspecified addresses at connection time. Loopback and private networks are
allowed for internal identity providers.

## Admin routing settings

These routes require an admin session. Bodies use the normal JSON and same-origin
checks; drafts accept up to 256 KiB.

- `GET /portal/api/admin/settings/routing` returns `settings` (the managed view for
  the routing domain: `managed`, `version`, effective `models` and `aliases`,
  `active`/`draft` revisions with `document` `{models, aliases}`, `validation` and
  `checks`, informational file `conflicts` and `history`), `runtime.revision` (the
  revision this process runs; `0` for file and per-model endpoint revisions),
  `credential_references`, `allowed_private_networks` and `group_grants`
  (group → granted models, `*` for all; file-owned).
- `POST …/import` with `{}`; afterwards `PUT /portal/api/admin/endpoints` returns 409.
- `PUT …/draft` with `{"version":N,"document":{"models":{…},"aliases":{…}}}`. Model
  objects use the YAML field names (`protocols`, `upstream_model`, `timeout_s`, `pool`
  with `url`, `health_path`, `weight`, `max_inflight`, `bearer_token_env`, …).
- `POST …/discard` with `{"version":N}`.
- `POST …/checks` with `{"revision":R}` (0: the current routing) returns `{revision,
  results}` with one `models.<name>.pool[<i>]` result per probed replica, or
  `destination` failures without probing.
- `POST …/activate`, `POST …/rollback` with `{"version":N,"revision":R}` and
  `POST …/apply` with `{}` publish to this router at once; activation needs the
  changed models' replicas to pass health checks.

## Models and client URLs

`GET /portal/api/models`:

```json
{
  "router_url":"http://localhost:8080",
  "base_url":"http://localhost:8080/v1",
  "models":[
    {"id":"chat-model","canonical_model":"chat-model","protocols":["openai","anthropic"]},
    {"id":"claude-sonnet-4-5","canonical_model":"chat-model","protocols":["openai","anthropic"]}
  ]
}
```

Models and aliases are sorted by ID and filtered by the user's effective model policy.
The origin is configured, never derived from untrusted Host or forwarding headers.

## Deployment settings

`OIDC_REQUIRED_CLAIM=name=value` refuses portal logins whose ID token lacks the claim value
(403, no user created). `OIDC_AUDIENCE` enables access tokens on `/v1/*`; `OIDC_SYNC_CLIENT_ID` and
`OIDC_SYNC_CLIENT_SECRET` enable the group sync job (see the runbook).
`OIDC_ISSUER`, `OIDC_PORTAL_CLIENT_ID`, `OIDC_PORTAL_CLIENT_SECRET`, `OIDC_GROUPS_CLAIM`
(default `groups`) and `OIDC_USERNAME_CLAIM` (default `preferred_username`) override the
optional YAML `oidc` section. The portal is always mounted; setting an issuer enables SSO.
Discovery and retry run independently of listening, with availability polled every ten
seconds. API-token and group-sync initialization retry without blocking local login/API keys.
`PORTAL_BASE_URL` sets
its public HTTP(S) origin (default `http://localhost:8080`); HTTPS is required when secure
cookies are enabled; cookie-issuing login operations return 503 for an HTTP origin with
secure cookies, while the API server keeps running. `PORTAL_COOKIE_SECURE` defaults to
true. Discovery supplies all OIDC
endpoints, including logout. Discovery without a logout endpoint is refused.

Compose alone sets `OIDC_BACKCHANNEL_URL=http://keycloak:8080/realms/olr-dev`: server-side
requests to the public issuer's subtree are transported to that internal URL. The expected
issuer and browser URLs stay public and are verified without exceptions. Remove this
override when the public issuer is directly reachable. Switching providers requires only
environment settings, plus registering the configured `/portal/callback` redirect and
`/` post-logout redirect with that provider.


### Group policies in routing revisions

The routing document includes `groups`, a map of group names to canonical
`models`, optional positive `tokens_per_day`, `tokens_per_month`, `rpm`, `tpm`,
`max_concurrent`, `max_concurrent_batch`, plus `strict_quota`, `priority`, and
`admin`. Null/omitted numeric limits are unlimited; omitted batch concurrency
inherits interactive concurrency. A group requires at least one model grant.
Routing draft validation checks model references and refuses removing groups
that still have stored memberships. `affected_users` and `impact_available` in
the routing view describe the pending draft's impact before activation.

`POST /portal/api/admin/settings/routing/preview` accepts a `document` or
`revision` (0 for active), optional `user_id`, and optional `memberships` array.
Omitting memberships uses the selected user's stored memberships. It returns
canonical `models`, allowed `aliases`, `limits`, `priority`, `admin`, stored
`overrides`, `protected_admin`, and `affected_users` IDs/usernames. The same
resolver enforces inference and portal access. Ordinary users receive 403.
The request uses the same JSON/Origin protection as other admin mutations.
