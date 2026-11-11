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
  "limits": {"tokens_per_day":2000000,"tokens_per_month":null,"rpm":60,"max_concurrent":4}
}
```

Limits resolve from the current stored groups; null means unlimited. An empty group list
is an empty JSON array and grants no models unless a matching configured group exists.

## API keys

- `GET /portal/api/keys`: `{"keys":[{"id":"uuid","prefix":"olr-...","label":"laptop",
  "created":"RFC3339","last_used":null,"expires":"RFC3339","revoked":false}]}`.
  Only the session user's keys are listed. Neither hashes nor full keys are returned.
- `POST /portal/api/keys`: accepts `{"label":"laptop"}` (1–100 bytes after trimming).
  Returns 201 `{"key":"olr-..."}` exactly once. Lifetime uses `auth.default_key_lifetime_days`.
  Refresh the key list for metadata after creating a key. Unknown fields are rejected.
- `DELETE /portal/api/keys/{uuid}` with `{}`: revokes an owned key, returning 204 with no body.
  Other users' and nonexistent keys return the same 404. Revocation clears API authentication
  caches before success, so the next API-key request is rejected. Repeated owned revocation
  is idempotent. Existing sessions do not authorize `/v1/*` requests.

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

`quota` has the same contract as `/v1/me/usage`, including live in-memory charges. `today`
and `month` are sorted per-model database aggregates, empty arrays without usage, and may
lag live charges while the asynchronous writer drains. Requests include recorded failures.
Calendar boundaries use `limits.timezone`. TTFB averages exclude missing measurements;
throughput averages include only measured streams with positive output/duration. Missing
averages are null. All queries are scoped to the session user.

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
