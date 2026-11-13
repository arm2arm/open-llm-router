# Security review — first release

Review for [#19](https://github.com/arm2arm/open-llm-router/issues/19), 11 November 2026, of
`main` at `d3a41db`: `internal/auth`, `internal/proxy`, `internal/limits`, `internal/portal`,
`internal/server`, `internal/home`, `internal/metrics`, the web app in `web/`, the Dockerfile,
Compose files and CI.

Method: an automated review (Claude Code) of the full source, not a second person. Each
checklist item was checked against the code and its tests; each finding below has a
regression test. A human review by someone who did not write the code is still recommended
before real users get keys, and is a condition of the pilot in
[#21](https://github.com/arm2arm/open-llm-router/issues/21).

## Findings

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | High | Model allow-list bypass through JSON key spelling | fixed |
| 2 | Medium | No deadline on request bodies | fixed |
| 3 | Medium | Public login route could fill the database | fixed |
| 4 | Low | Backend `Set-Cookie` reached the router origin | fixed |
| 5 | Low | Base images pinned by tag only; no update automation | fixed |
| 6 | Info | Accepted risks | documented |

### 1. Model allow-list bypass through JSON key spelling (high)

The proxy read `model` and `stream` with `encoding/json` into a struct. That decoder matches
field names without regard to case and keeps the last duplicate, while the backend (Python
in vLLM and SGLang, Go in Ollama, others) reads the exact key. A caller allowed only
`allowed` could send `{"model":"secret","MODEL":"allowed"}`: the router authorized and routed
`allowed`, forwarded the body unchanged, and a backend that serves both names (Ollama, vLLM
with several `--served-model-name` values or LoRA adapters) answered with `secret`.

Fix: `internal/proxy/request.go` reads the routing fields in one pass and answers 400 when a
top-level key matches `model` or `stream` only without regard to case, or appears twice.
Tests: `TestParseRequestRefusesAmbiguousRoutingFields`,
`TestAllowListCannotBeBypassedByKeySpelling` (two models on one backend; nothing reaches it).

### 2. No deadline on request bodies (medium)

The server sets `ReadHeaderTimeout` but, deliberately, no `ReadTimeout`, because that would
also end streamed responses. A body could therefore be sent arbitrarily slowly: an
authenticated client could hold many connections, each with a goroutine and up to 32 MiB of
buffer, and any client could stall a connection on a route that does not read its body.

Fix: the server sets a read deadline of `server.RequestBodyTimeout` (2 minutes) on every
request with a body. The proxy clears it once the body is read, so responses can stream for
the model's `timeout_s`; a body that is not complete in time gets 408 and the connection is
closed. Tests: `TestSlowRequestBodyTimesOutButLongResponsesStream`,
`TestUnreadBodyOnOtherRoutesIsBounded`.

### 3. Public login route could fill the database (medium)

Every `GET /portal/login` stores a one-use login challenge for ten minutes. The route is
unauthenticated, so a loop could insert rows without bound.

Fix: at most `portal.MaxPendingLogins` (10,000) unexpired challenges exist; further logins get
503 with `Retry-After: 60` until challenges expire or complete. A flood can delay logins for
up to ten minutes but can no longer exhaust storage; rate limiting per client address belongs
in the TLS reverse proxy (runbook). Test: `TestPortalPendingLoginsAreCapped`.

### 4. Backend `Set-Cookie` reached the router origin (low)

Backend response headers were copied to the client apart from hop-by-hop headers. The router
origin also serves the portal, so a misbehaving backend could set a cookie such as
`olr_session` on `/portal`. Fix: `Set-Cookie` is removed from proxied responses.
Test: `TestBackendCannotSetCookiesOnRouterOrigin`.

### 5. Base images pinned by tag only (low)

The Go module and the npm lockfile were pinned, GitHub Actions by commit SHA, but the base
images by mutable tag (`distroless/static-debian13:nonroot` in particular). Fix: the
Dockerfile pins each base image by digest, and `.github/dependabot.yml` proposes weekly
updates for Go modules, npm, Docker digests and Actions.

### 6. Accepted risks (info)

- API keys are attributable to users and expire (`auth.default_key_lifetime_days`);
  issuance now also enforces the site maximum lifetime and active-key count (default 10).
- `/readyz` is public and pings the database, bounded to 2 s per call. Do not route it through
  the public TLS proxy if that matters to the site.
- Request log lines carry the user ID and the 12-character display prefix of the key; they
  never carry the key, prompts or responses. Keep the log retention at or below the usage
  retention (runbook).
- Groups of API-key users refresh at portal login and through the configured Keycloak
  sync job; an outage delays refresh, so disable an account locally when urgent.
- Rate limiting per client address is left to the reverse proxy; the router does not trust
  forwarding headers.

## Checklist

- [x] Keys are generated with `crypto/rand` (32 bytes, base62), authenticated by
  SHA-256 hash, and never logged. Privileged recovery and ciphertext storage were
  removed for OLR-SEC-04; migration drops historical ciphertext without changing
  authentication hashes. `TestPrivilegedKeyRecoveryRemoved` verifies hash-only
  issuance even with a legacy encryption environment variable, missing ciphertext
  columns, denied non-admin/Origin requests and the retired admin endpoint.
- [x] Client credentials and cookies are never forwarded: `Authorization`, `X-Api-Key`,
  `Cookie`, `Proxy-Authorization`, baggage and hop-by-hop headers are dropped before the
  backend request; the backend bearer token is set only towards the backend and never
  returned. Backend `Set-Cookie` is now dropped (finding 4).
- [x] The model allow-list is enforced after alias resolution on every proxied path, before a
  replica is chosen (`TestPolicyDeniesBeforePoolAcquisition`), now with unambiguous field
  parsing (finding 1). Every `/v1` path, including unknown ones, passes the authentication
  middleware before the mux.
- [x] Backend URLs come only from the config, as origins without path or query. The proxy uses
  `Transport.RoundTrip`, which does not follow redirects; health checks and OIDC clients
  refuse redirects.
- [x] Body size limits: 4 MiB by default on the proxy, 8 KiB on the portal API. Timeouts: header 10 s,
  body 2 min (finding 2), backend connect 5 s, response headers `timeout_s`, idle between
  chunks `idle_timeout_s`, blocked client writes `idle_timeout_s`, portal requests 10 s,
  readiness 2 s, OIDC requests 10 s.
- [x] OIDC: `state` and nonce are random, one-use, stored hashed and bound to the browser by
  an HttpOnly cookie; PKCE S256; issuer, audience, expiry, nonce, `azp` and `at_hash` are
  checked; the redirect URI is fixed from `PORTAL_BASE_URL`.
- [x] Session cookie: `HttpOnly`, `SameSite=Lax`, `Path=/portal`, `Secure` by default (HTTPS
  base URL enforced). Sessions are stored hashed, rotated at login, deleted at logout and
  refused for disabled users. State-changing portal requests require an exact `Origin` and a
  JSON content type, and refuse cross-site Fetch Metadata.
- [x] The web app keeps no key or token in browser storage (no `localStorage`,
  `sessionStorage` or cookies from script), renders user input as React text only (no
  `dangerouslySetInnerHTML`), and is served with `default-src 'none'; script-src 'self'`.
- [x] A user can read, change and delete only their own keys and usage: every portal query is
  scoped by the session user; another user's key gives the same 404 as a missing one.
- [x] No prompts or responses in server logs, metrics or exported OpenTelemetry spans.
  Personal content traces in Postgres are disabled by default, with explicit user opt-in and
  1–30 day retention (default 7). Only the attributed user can read them, including
  against an admin session; deletion/opt-out invalidate pending writes. Tests cover
  ownership, expiry, erasure, bounded capture and omission of credentials/attachments.
  Protect database access and backups as content-bearing storage. See [REQUEST_TRACING.md](REQUEST_TRACING.md).
- [x] Dependencies pinned (`go.sum`, `web/package-lock.json`, Actions by SHA, base images by
  digest) and scanned: `govulncheck` reports no reachable vulnerability and `npm audit`
  reports none (both run in CI). The images run as the distroless `nonroot` user. No secrets
  in the image (`.dockerignore` admits only sources) or in Git; Compose credentials are public
  development defaults, marked as such.

### Follow-up: body buffering before admission (#113)

Uploads now acquire global and per-user slots before reading any bytes. Defaults are 32
uploads per process and 4 per user, with the user cap further reduced by the effective key
class concurrency. Different keys and classes share a user's upload budget. Slots are held
through JSON parsing, rewriting and quota admission, then released; all error/panic and
cancellation exits release them. Excess readers are rejected immediately (user: 429,
global: 503), with `Retry-After: 1` and no draining of the rejected HTTP/1 upload.
Declared oversized requests get 413 before any read; chunked requests have the same byte cap.

The body limit defaults to 4 MiB. These are pre-admission bounds, not a total process memory
budget: admitted requests, JSON copies, tracing and runtime overhead need separate headroom.
The existing inference concurrency and replica limits still govern admitted work.
`TestConcurrentLargeUploadsHaveFixedMemoryCeiling` holds a nearly full body for a user with
`max_concurrent: 1`, rejects 128 simultaneous uploads without reading them, checks the active
reader/byte ceiling and live heap, then verifies cancellation cleanup. Network regression
tests cover stalled fixed-length and chunked uploads and immediate oversized rejection.
See [edge configuration](RUNBOOK.md#behind-a-tls-reverse-proxy) for the matching limits.


## Local administration follow-up (#83–85)

13 November 2026. Automated source review by the implementing agent, backed by
regression/browser tests; this is not an independent human approval or the #21
production review. Scope: local credentials/recovery, portal authorization,
managed authentication/routing/group settings, audit transactions and new
installation/restore fixtures.

| Reviewed boundary | Finding or evidence |
|---|---|
| Clean no-IdP startup | Release-blocking install failure fixed: an empty `OIDC_ISSUER` created an invalid OIDC configuration even when YAML omitted OIDC. Empty/no-section now stays local-only; explicit invalid OIDC still fails closed. `TestEmptyIssuerKeepsAnOmittedOIDCSectionLocalOnly` and `make acceptance-local` cover both. The local-only override removes inherited OIDC environment bindings entirely so they cannot conflict with a later managed connection. |
| Protected identity/credentials | API, store and database deletion/disablement/demotion/credential guards; `policy.ForUser` preserves independent protected authority, grants and unlimited limits. Concurrent bootstrap, password rotation, reset refusal and recovery are tested in `local_admin*_test.go`. |
| Portal mutation authorization | Admin wrappers use active stored access; JSON mutations enforce same-origin requests. Authentication changes/checks/test sign-in require a recent local admin session; SSO and ordinary sessions are refused. Browser/API tests cover 403 and restricted first-change sessions. |
| Secret and token handling | Passwords use bounded Argon2id; session revisions invalidate rotated credentials; recovery revokes keys/sessions. Stored SSO secrets require the separate settings encryption key. Views/history expose metadata; tests assert no password/hash/key/client-secret/free-text reason in audit or logs. Browser capture artifacts remain disabled. |
| Policy activation races and rollback | Local assignment and routing activation share the database advisory lock; existing memberships block destructive group deletion. Model/grant/policy validation is atomic, previews use stored overrides, and in-flight requests retain their routing snapshot. Integration tests exercise refusal and rollback. |
| Audit consistency and restore | Injected audit-store failure rolls back sensitive credential/account/settings changes. History/revisions reject updates/deletion/truncation. Upgrade and full dump/restore preserve identity, credentials, active revisions and existing events; recovery is auditable. |
| External IdP trust | An isolated real Keycloak realm tests bad client credentials, inaccessible discovery, issuer mismatch, successful checked sign-in, provider outage/restart and rollback with local access intact. Destination/redirect/issuer protections remain enforced. |

No additional release-blocking finding was identified in this scoped review after
the startup fix. Existing operator trust, single-router scope, history retention,
TLS/edge throttling and DBA/superuser limitations still apply. Independent review
and the target deployment's production acceptance remain required separately.
