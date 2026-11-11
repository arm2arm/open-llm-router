# Portal backend API

The portal is served at the configured `PORTAL_BASE_URL` origin. `/portal/*` uses an
opaque eight-hour session cookie; `/v1/*` continues to require API keys and ignores cookies.
All portal responses use `Cache-Control: no-store`. Errors are JSON:
`{"error":{"type":"authentication_error","message":"authentication required"}}`.
Unauthenticated API calls return 401; forbidden origins/disabled logins return 403;
invalid input returns 400, unknown or other users' keys return 404, and unavailable
dependencies return 503. No OIDC token is stored or returned to the browser.

## Login and logout

- `GET /portal/login`: starts authorization code login with fresh state, nonce and PKCE S256.
  A ten-minute HttpOnly cookie binds the one-use server-side challenge to this browser.
  At most 10,000 unfinished challenges exist at a time; beyond that the route answers 503
  with `Retry-After: 60`.
- `GET /portal/callback`: consumes the challenge, exchanges the code and validates the ID
  token's signature, issuer, audience, expiry and nonce. The user is upserted by `(issuer, sub)`;
  email is never an identity key. Username, email and groups refresh on each login. Disabled
  users cannot log in. A successful login rotates the session and redirects to `/portal/`.
- `POST /portal/logout` with `{}`: deletes this session and clears its cookie. Returns
  `{"logout_url":"<discovered end_session_endpoint with client_id and post_logout_redirect_uri>"}`.
  The UI navigates to that URL to end the identity-provider session. Keycloak asks for
  confirmation because no ID token is retained or exposed as an `id_token_hint`.

Session cookies are `HttpOnly; SameSite=Lax; Path=/portal`, with `Secure` on by default.
Only local HTTP development explicitly sets `PORTAL_COOKIE_SECURE=false`.
State-changing requests, including logout and DELETE, require `Content-Type: application/json`
and an `Origin` exactly matching `PORTAL_BASE_URL`; cross-site Fetch Metadata is rejected.
The UI can use `fetch(url, {method, headers: {'Content-Type': 'application/json'}, body: ...})`.

## Current user

`GET /portal/api/me`:

```json
{
  "user": {"id":"uuid","username":"staff","email":"staff@example.test","groups":["staff"]},
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

Members of a group with `admin: true` in the config get `"admin": true` in `/portal/api/me` and
these routes; everyone else gets 403. Membership is read from the stored groups on each request.

- `GET /portal/api/admin/users?match=text`: up to 500 users whose username or email contains the
  text (case-insensitive), with groups, enabled flag, active key count, tokens today and this
  month, and overrides by field.
- `GET /portal/api/admin/users/{id}`: the user, effective `limits`, `overrides` with `reason`,
  `set_by` and `set_at`, and key metadata.
- `PUT /portal/api/admin/users/{id}/overrides/{field}` with `{"value":N,"reason":"…"}` (`null` for
  unlimited) sets an override, recorded as `set_by: "portal:<admin username>"`;
  `DELETE …/overrides/{field}` with `{}` clears it. Both return 204 and take effect at the user's
  next request on every router.
- `PUT /portal/api/admin/users/{id}/enabled` with `{"enabled":false}` disables a user (sessions
  and keys stop working at once) or re-enables them; admins cannot disable themselves.
- `GET /portal/api/admin/groups`: per configured group, enabled members, requests and tokens today
  and tokens this month. A user in several groups counts in each.
- `GET /portal/api/admin/replicas`: per model, each replica's origin, health, in-flight calls,
  `max_inflight` and weight.

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

`OIDC_AUDIENCE` enables access tokens on `/v1/*`; `OIDC_SYNC_CLIENT_ID` and
`OIDC_SYNC_CLIENT_SECRET` enable the group sync job (see the runbook).
`OIDC_ISSUER`, `OIDC_PORTAL_CLIENT_ID`, `OIDC_PORTAL_CLIENT_SECRET`, `OIDC_GROUPS_CLAIM`
(default `groups`) and `OIDC_USERNAME_CLAIM` (default `preferred_username`) override the
optional YAML `oidc` section. Setting an issuer enables the portal. `PORTAL_BASE_URL` sets
its public HTTP(S) origin (default `http://localhost:8080`); HTTPS is required when secure
cookies are enabled. `PORTAL_COOKIE_SECURE` defaults to true. Discovery supplies all OIDC
endpoints, including logout. Discovery without a logout endpoint is refused.

Compose alone sets `OIDC_BACKCHANNEL_URL=http://keycloak:8080/realms/olr-dev`: server-side
requests to the public issuer's subtree are transported to that internal URL. The expected
issuer and browser URLs stay public and are verified without exceptions. Remove this
override when the public issuer is directly reachable. Switching providers requires only
environment settings, plus registering the configured `/portal/callback` redirect and
`/portal/` post-logout redirect with that provider.
