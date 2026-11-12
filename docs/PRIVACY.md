# Privacy notice — template

This is a template for sites that run open-llm-router. Replace every `<…>` with the site's
value and publish the result where users sign in. It describes what the router itself stores;
the model backends, the reverse proxy and the identity provider are covered by their own
notices.

---

**Controller:** `<organization, address>` · **Contact for privacy requests:** `<email or form>`

## What is stored

When you use the LLM router at `<https://router.example.org>`, it stores:

| Data | Content | Kept for | How you delete it |
|---|---|---|---|
| Account | identity-provider issuer and subject, username, email, group memberships, enabled flag, last login | while you have access | ask `<contact>`; the operator deletes the account with everything below |
| API keys | label, first 12 characters, a one-way hash, optional encrypted copy for administrator recovery, creation, last use, expiry, revoked flag | until you remove them | revoke a key in the portal, then remove it for good |
| Sessions | a hash of the session cookie, creation and expiry | 8 hours, or until you sign out | sign out |
| Per-request records | time, model, replica, protocol, status, input and output token counts, latency, time to first token, the key used, and the end user if a trusted application made the call for you | `<90>` days | **Usage → Delete my activity**, at any time |
| Detailed request traces | message text, reasoning, tool calls/results, provider errors, selected parameters and stream event timings; enabled by default, user can disable | 7 days by default; user chooses 1–30 days | **My requests → Delete traces** or **Usage → Delete my activity** |
| Daily totals | per day and model: number of requests and tokens | `<400>` days | not deletable by you: needed to enforce today's and this month's quotas and for accounting; the operator deletes them with your account |

**Content capture:** detailed tracing is enabled by default, including existing accounts.
Users control it on **My requests**. Text and tool arguments may contain sensitive data;
there is no automatic secret detector in that text. Disabling stops new capture and
invalidates pending captures; existing detail remains until expiry or deletion. Shorter
retention applies to existing detail; longer retention applies only to new captures.

**Omitted from traces:** HTTP headers, credentials, cookies, provider signatures, arbitrary
metadata, image/document/audio payloads, attachment URLs and embedding vectors. Content
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
  to `<tracing backend>`. Spans carry no user identifier unless the site enables it
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
processed on `<hosting location>` and is not shared with third parties `<or: list recipients>`.

---

## For operators

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
- Keep log retention at or below the per-request retention. Exported traces must be erased in
  the tracing backend separately.
- Who answers erasure requests and the retention periods are decisions for the site
  ([#21](https://github.com/arm2arm/open-llm-router/issues/21)).
