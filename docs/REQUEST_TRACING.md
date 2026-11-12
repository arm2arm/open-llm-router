# Personal request tracing

Open **My requests** (`/portal/requests`) to inspect LLM calls made with your keys,
OIDC access token, or by a trusted application on your behalf. Detailed logging is
**enabled by default**, including for existing accounts. Retention defaults to
**7 days**; each user can choose **1–30 days** and save their own preference.

## Use the trace viewer

1. Set **Capture enabled/disabled** and **Keep traces for**, then **Save settings**.
2. Make a call using an OpenAI or Anthropic client. Completed requests appear in the
   table, which refreshes every 5 seconds while the page is open. Background capture
   becomes available shortly after completion. Existing historical requests cannot
   be backfilled.
3. Select **Inspect**. **Conversation** shows input/output messages, collapsible
   reasoning, tool calls with reconstructed arguments, and tool results in input
   messages. The router forwards tool calls; the client executes tools and submits
   their results in subsequent requests. It does not execute or trace tools itself.
4. **Stream events** shows text, reasoning and tool argument deltas, relative arrival
   times, completion and provider errors. Filter by event type. Events appear in
   pages of 100. This is a completed-request replay, not a live token console.
5. **Details** shows the full request ID, operation, expiry and selected generation
   parameters. Correlate the ID with `X-Request-Id` in the response and server logs.
   Search the table by model, protocol, status, operation or request ID; filter for
   streams, failures or requests with detail.

Timing cards show total latency, time to first byte (labelled first token), token
counts and estimated output speed. Event times measure when the router observed delivery to the client, including
backpressure; several events in one network read can have the same timestamp. They
are not provider token-generation timestamps. Interrupted calls keep their partial,
complete SSE events and have `aborted` status; an SSE error has `error` status.

## Privacy and retention

- Content is private to the **attributed user**. Admin privileges do not permit
  reading another account's detailed traces. A trusted forwarder's owner cannot
  read traces attributed to the forwarded user. Database operators/backups still
  have access: protect Postgres, disk encryption and backups accordingly.
- Captured text includes prompts, responses, reasoning, tool arguments/results and
  provider error messages. It may contain sensitive information. There is no
  automatic secret detector inside user text. Disable capture before sending
  content you do not want retained.
- HTTP headers, credentials, cookies, provider signatures, arbitrary metadata,
  image/file/audio payloads, attachment URLs and embedding vectors are omitted.
  Images, documents and audio appear as markers. Supported text/reasoning/tool
  fields are normalized; unknown provider-specific content fields are omitted.
- Disabling capture stops new content collection on that router immediately and
  invalidates pending/in-flight captures. Existing traces remain until their
  original expiry or explicit deletion. On another router, preference snapshots
  refresh every 15 seconds; the database always rechecks opt-out and generation
  before persisting a capture. A stale instance cannot save opted-out content.
- Shortening retention shortens existing traces too. Extending it applies to new
  traces and does not extend older expiry dates or recover deleted content.
- Expired detail is immediately inaccessible. A separate cleanup deletes up to
  1,000 expired traces every 15 seconds. Cleanup failure/backlog does not make
  expired content readable. Retention applies to the live database; backup
  expiry is an operator responsibility.
- **Delete traces** deletes only content, preserving request metadata and totals.
  **My usage → Delete my activity** removes both per-request metadata and content.
  Both invalidate pending captures. Daily quota totals remain. Deleting the user
  cascades to all their traces. Exported OpenTelemetry spans are separate.
- The browser fetches content only when opening a trace, releases its query cache
  on close, and does not persist content to local/session storage. Trace responses
  use the portal's `no-store` policy and normal full-session/Origin protections.

## Performance and operational limits

The forwarding path performs no tracing SQL and no content JSON decoding. It copies
bounded bytes after writing to the client; two background workers normalize content
and write it to `request_traces`, independently of the usage/accounting queue.
Streaming responses still flush incrementally. Tracing neither retries nor delays
an inference call when its storage queue is full or Postgres fails.

Bounds per request are **128 KiB input**, **256 KiB output**, **2,048 observed writes**
and **1,000 displayed stream events**. At most **32 captures** can be active or queued
per process. Input over the cap is omitted as a whole; output keeps only its captured
prefix and complete events. The viewer explains truncation; forwarding is unaffected.
Large binary-containing requests may therefore have no input detail even though their
output is captured. Parsing/serialization has additional bounded worker memory/CPU
cost. No external telemetry SDK or service is required for personal tracing.

Capacity pressure skips new traces rather than blocking inference. The table shows
**No detail** for skipped, disabled, expired, pending and pre-upgrade requests.
Metadata-only `request traces skipped` warnings report cumulative `dropped` and
`failed` counts, at most once every 15 seconds. Storage operations have a 2-second
deadline. Shutdown drains the proxy first, then allows up to 2 seconds for content
writes; pending content can be lost during shutdown or outages. Quota accounting
continues through its existing independent queue and shutdown flush.

Supported calls are `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` and
`/v1/messages`. Models/discovery/count-tokens and authentication failures are not
personal content traces. Router admission errors and upstream errors are traced for
authenticated, attributed inference requests. Usage records retain their separate
site-configured lifetime (90 days by default), daily totals 400 days by default.

See [PORTAL_API.md](PORTAL_API.md#personal-trace-settings-and-content) for the API,
[PRIVACY.md](PRIVACY.md) for the notice template, and
[OBSERVABILITY.md](OBSERVABILITY.md) for metadata-only metrics, logs and OTLP spans.
