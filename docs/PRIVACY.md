# Privacy notice — template

This is a template for sites that run open-llm-router. Replace every `<…>` with the site's
value and publish the result where users sign in and in the clients they use. Complete the
recipient and backend-retention information below as well as the router's storage periods;
link the actual notices for the model backends, reverse proxy and identity provider.

---

**Controller:** `<organization, address>` · **Contact for privacy requests:** `<email or form>`

## AI interaction and inference recipients

You interact with an AI model through `<client names, e.g. DRPHub, Hermes, an IDE,
Open WebUI>`. Model responses can be inaccurate. Check important information.
The client sends `<describe prompts, history, system instructions, tool definitions,
tool results and included attachments>` through the router to a model backend.
The router can send the full request to any permitted replica on initial selection
and to another replica on a qualifying retry. An initial recipient may already have
processed the request before a failed response causes a retry. Routing is not
restricted to one organization merely because a model has one public name.

List **every possible primary and retry recipient**, including replicas that are
temporarily unhealthy. Expand aliases to their active model pools. For each model
or alias available to this audience, complete:

| Model / aliases | Recipient organization and role | Initial / retry destination | Processing location and transfers | Backend logging, training use and retention | Actual privacy notice / contact |
|---|---|---|---|---|---|
| `<model and aliases>` | `<organization / processor or controller>` | `<initial, retry, or both>` | `<location, transfer safeguards>` | `<prompt/output/error logs, training use, retention periods, deletion and backups>` | `<published URL / contact>` |
| `<same model, additional replica organization>` | `<organization / role>` | `<initial and retry>` | `<location, safeguards>` | `<logging, training, retention and deletion>` | `<published URL / contact>` |

**Routing and storage are separate.** Turning off personal request tracing only
stops the router's optional content capture. It does not stop inference requests,
full-request retries, normal accounting, client-side history, or backend logging.
Deleting router activity does not delete copies in clients or at backend providers;
describe those deletion routes here: `<client/backend contacts and procedures>`.

## What is stored

When you use the LLM router at `<https://router.example.org>`, it stores:

| Data | Content | Kept for | How you delete it |
|---|---|---|---|
| Account | identity-provider issuer and subject, username, email, optional full/first/last name and institution supplied at institutional sign-in, group memberships, enabled flag, last identity update and when access was disabled | until operator erasure, including after access is disabled or employment/membership ends; there is no automatic disabled-account expiry | ask `<contact>`; the operator deletes the account and its owned records; administration history remains as described below |
| API keys | label, first 12 characters, a one-way hash, creation, last use, expiry, revoked flag | until you remove them | revoke a key in the portal, then remove it for good |
| Known local devices | one-way hash of a random `olr_device` cookie, account ID, creation and expiry; no IP address or browser fingerprint | 30 days after successful local sign-in; expired rows removed by daily retention | clear the cookie in your browser; account erasure removes registrations |
| Sessions | a hash of the session cookie, creation and expiry | 8 hours, or until you sign out | sign out |
| Administration credentials and history | administration-key owner, permissions, approved networks, creation/expiry and last use; last-use source network is IPv4 `/24` or IPv6 `/48`, never a precise host address; events retain actor/target UUIDs, action, result, reason, timestamps, revision and coarse agent source network, plus operator CLI account/host attribution | credentials until owner erasure; append-only events indefinitely unless the site adopts a documented archive/retention policy; usage retention does not prune history | account erasure removes owned credentials and previews; historical attribution requires authorized offline privacy maintenance, also covering exports, logs and backups |
| Agent previews | operation, argument binding hash, state/policy version, parameter/field names, status and timestamps; before/after identity values, key labels and free-text reasons are not retained | unused previews expire after 10 minutes; completed or interrupted changes retained for 1 day; physical cleanup runs at startup and daily, so deletion can lag by up to 1 day plus any cleanup backlog | deleting the administration-key owner removes their credentials, delegated descendants and preview rows; permanent audit events remain separately |
| Per-request records | time, model, replica, protocol, status, input and output token counts, latency, time to first token, the key used, and the end user if a trusted application made the call for you | `<90>` days | **Usage → Delete my activity**, at any time |
| Detailed request traces | message text, reasoning, tool definitions, calls and results, provider errors, request parameters, sanitized raw request/response previews, routing steps (pool position, waits, forwarded header names) and stream event timings; disabled by default; requires site approval and personal opt-in | 7 days by default; user chooses 1–30 days | **My requests → Delete traces** or **Usage → Delete my activity** |
| Daily totals | per day and model: number of requests and tokens | `<400>` days | not deletable by you: needed to enforce today's and this month's quotas and for accounting; the operator deletes them with your account |

The local-device cookie helps keep sign-in available during password-guessing
attacks. It remains after sign-out and password rotation, but cannot log you in,
authorize requests or replace your password. Clearing it returns the browser to
the anonymous guessing budget.

**Content capture:** detailed tracing is disabled by default. Site approval and individual opt-in are both required.
Users control it on **My requests**. Text and tool arguments may contain sensitive data;
supplemental heuristic redaction cannot guarantee removal of all secrets. Disabling stops new capture and
invalidates pending captures; existing detail remains until expiry or deletion. Shorter
retention applies to existing detail; longer retention applies only to new captures.
Stream text is reconstructed before redaction. Timeline events retain types and
timing; raw frames omit free-text fragments so secrets split across deltas cannot
be recovered from them. Structured credential fields are removed from parameters,
raw previews and valid JSON tool arguments. NUL characters become `�` in stored traces.

**Omitted from traces:** HTTP header values, credentials, cookies, provider signatures,
`metadata` fields, image/document/audio payloads, data URIs, attachment URLs, backend
addresses and embedding vectors. Content
traces are accessible only to the attributed user, including when an application forwards
a call. Portal admins have no override. Database and backup operators can access content.
See [REQUEST_TRACING.md](REQUEST_TRACING.md) for capture limits and supported formats.

Deleting your activity records does not reset your quotas: the counters of the current day
and month stay in effect.

## Outside the database

- **Request logs.** Each request writes one log line with the request ID, your user ID, the
  12-character key prefix, model, status, token counts and timing — no content. Logs are kept
  for `<14>` days, which is not longer than the per-request records.
- **Metrics.** Aggregated counts and timings per model and replica. They contain no user or key.
- **OpenTelemetry spans** (if enabled, separate from personal content traces). One span per call with model, replica, token counts and timing, sent
  to `<tracing backend>`. Spans include group names in `olr.groups`, which may identify
  a team, department or membership even when user IDs are disabled. Spans carry no user identifier unless the site enables it
  (`OLR_TRACE_USER_ID`, `<enabled/disabled>` here). Spans already exported are kept there for
  `<…>` days and are not removed by deleting your records.
- **Identity provider.** Signing in goes through `<Keycloak realm / institution>`; its own notice
  applies.

## Your rights

You can see your keys and usage in the portal at `<https://router.example.org/portal/>`, and
delete your activity records and removed keys yourself. For a copy of your data, a correction,
or erasure of your account, contact `<contact>`. Requests are answered within one month.

## Legal basis and recipients

`<Legal basis, e.g. performance of a service for members of the organization>`. Data is
processed on `<router hosting location>` and sent to the inference recipients listed
above. Also list `<other recipients, locations, roles and applicable safeguards>`.
Authorized administrators can inspect account identity, groups, limits and usage/key
metadata. If the site enables agent administration and grants `users:read`, the
authorized agent client and its operator are recipients of usernames, email
addresses, memberships, limits and key metadata; include those recipients in the
site's notice and access review. This does not grant access to personal content
traces through the administration API. Database/backup operators and authorized
data-export operators can access stored content.

---

## For operators

- Publish the completed site notice and set `portal.privacy_notice_url` to its
  absolute HTTPS URL (up to 2048 bytes, no credentials, query or whitespace).
  A nonempty `PORTAL_PRIVACY_URL` overrides the file setting. When both are empty,
  sign-in and Connect remain usable and displays a contact-operator message. The router
  validates the link's syntax; the operator owns its content and availability.
  Compose forwards this environment variable; Helm accepts it under
  `env.PORTAL_PRIVACY_URL`. Restart the router after changing it.
- In **Backends**, set **Public organization** and **Recipient privacy URL** for
  each backend. For file-configured/manual replicas, set `public_organization`
  and `privacy_url` on every `models.<name>.pool[]` entry. Managed backend metadata
  replaces replica metadata when a backend reference is resolved. These fields
  are public: do not put credentials, internal hostnames, private backend paths
  or administrator-only names in them. They are never inferred from connection
  settings. Missing labels/notices are explicitly incomplete in Connect and the
  authenticated catalogues. Existing `portal.recipient_labels` entries remain
  a fallback when a replica has no public organization; they do not supply a
  recipient privacy URL. Review them whenever routing or access changes.
  If a backend is itself a gateway, confirm its onward recipients and include
  them in the site notice.
- Put the [first-session AI notice](CLIENTS.md#first-session-ai-notice) in each
  client where users actually send prompts, including users who never visit the
  portal. The sign-in privacy link and Connect explanation do not replace it.
- Per-request retention is `retention.usage_days` (default 90) and daily totals
  `retention.usage_daily_days` (default 400) in the config file; put the same values in this
  notice. The router deletes older rows at startup and every night at `retention.run_at`, and
  the portal's usage page states both periods.
- Personal trace expiry is enforced on every read; physical cleanup deletes bounded batches
  every 15 seconds. Keep backups consistent with your site's policy; deleting live rows
  does not erase backup copies.
- Erasure on request: `olr admin user list -match <name or email>` finds the user,
  `olr admin user delete -id <uuid>` deletes the account, keys, sessions, per-request records
  (including calls forwarded for that user) and daily totals in one transaction. See
  [RUNBOOK.md](RUNBOOK.md#erase-a-user-on-request).
- Data access on request: `olr admin user export -id <uuid> -output NEW_PRIVATE_FILE`
  writes a private JSON snapshot of account data, credential metadata without
  hashes/secrets, overrides, sessions/device timestamps, attributed requests,
  daily totals, sanitized personal traces and relevant administration events.
  Forwarded calls belong to their end user, not the forwarder owner's export.
  The file is sensitive: transfer it through an approved channel and delete the
  working copy afterward. Requests already expired or deleted are absent.
- Disabling access or removing an identity at the IdP does not erase the router
  account. Set a site policy of `<number>` days after disabling, and schedule an
  operator review with `olr admin user list -disabled -older-than 2160h` (90 days).
  Review candidates and erase individually; re-enabling resets this age. Existing
  disabled accounts begin their age at migration 0028 because older disable times
  were not recorded. Document any lawful reason for longer retention.
- Include audit history, coarse source networks, group-bearing exported spans,
  agent-client recipients, exports, archives and backup expiry in the site's
  privacy assessment and retention procedure. Live account erasure preserves the
  historical attribution described above and cannot retract already exported data.
- Keep log retention at or below the per-request retention. Exported traces must be erased in
  the tracing backend separately.
- Who answers erasure requests and the retention periods are decisions for the site
  ([#21](https://github.com/arm2arm/open-llm-router/issues/21)).
