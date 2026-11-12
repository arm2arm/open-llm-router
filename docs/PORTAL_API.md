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
  with `local: {available, login_url, password_url}` and
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
  A local session returns `{"logout_url":"/portal/login/local"}` without contacting the
  provider. SSO sessions use the provider URL when available and fall back to the local
  URL during an outage. The UI navigates to that URL. Keycloak asks for
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
  `expires_in_days` is optional, an integer from 1 to 36500. Omission uses
  `auth.default_key_lifetime_days`. Returns 201 `{"key":"olr-..."}` exactly once.
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
  "ttfb_ms":50,"stream":true,"key_prefix":"olr-…","forwarded":false}]}`, newest first, the
  user's last 200 records (metadata only). `key_prefix` is null once the key is removed, and
  for calls a trusted forwarder made for the user (`forwarded: true`).
- `GET /portal/api/usage/daily`: `{"timezone":"UTC","days":[{"day":"2026-10-13","requests":0,
  "input_tokens":0,"output_tokens":0}, …]}`: exactly 30 days ending today in `limits.timezone`,
  oldest first, from the daily totals (unaffected by deleting activity).

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
  `expires`, `revoked`, `expired`, `class` and `trusted_forwarder`, never hashes or raw keys.
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
  next API-key request on every router. Cached OIDC access-token principals can
  take up to one minute to refresh.
- `PUT /portal/api/admin/users/{id}/enabled` with `{"enabled":false}` disables a user (sessions
  are deleted and API-key access stops working at once) or re-enables them; re-enabling
  requires a fresh browser login and restores only unrevoked, unexpired keys. Cached OIDC access-token
  principals can remain valid for up to one minute. Admins cannot disable themselves.
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
`/portal/` post-logout redirect with that provider.
