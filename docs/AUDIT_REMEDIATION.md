# Audit remediation ledger

The audit in [#154](https://github.com/arm2arm/open-llm-router/issues/154)
consolidated 110 findings into 20 work packages. Closing a source issue for
consolidation did not resolve its finding. Each package stays open until all
transferred acceptance criteria have evidence or an explicit disposition.

This pass starts from `5cb417d` on 17 November 2026. Existing completed packages
are #155, #179, #158, #159 and #162. Work proceeds through P1 packages before P2
and P3; dependencies may change that order.

| Package | Source findings | Progress in this pass |
| --- | --- | --- |
| #172 Authentication and identity sync | #157, #161, #172, #187, #198, #252, #253 | PRs #292, #293, #294 |
| #165 Usage accounting | #165, #168, #178, #204 | Completed in PR #295 |
| #166 Trace privacy and bounds | #166, #182, #214, #215, #216, #233, #251, #260 | Completed in PRs #296, #297, #298 and #299 |
| #169 History and user erasure | #169, #170, #213, #239, #244, #248 | Completed across PRs #300–304 |
| #164 Quotas and admission | #164, #176, #184, #189, #203, #249 | Strict request bounds: PR #305; calendar retry guidance: PR #306; explicit override values: PR #307; Batch inheritance, reservation feedback and TPM transitions: PR #308 |
| #181 Managed routing | #181, #207, #208, #210, #211, #254, #255, #257 | Pending |
| #173 Request history | #173, #174, #229, #258 | Pending |
| #185 Accessibility | #185, #230, #250, #264 | Pending |
| #226 Portal key and form UX | #201, #226, #227, #228, #262, #263 | Pending |
| #195 Agent administration | #195, #245, #261 | Pending |
| #167 Deployment reliability | #167, #188, #191, #192, #217, #218, #219, #240 | Pending |
| #183 Releases | #183, #193, #220, #221, #225, #242 | Pending |
| #237 Startup diagnostics | #237, #238, #241 | Pending |
| #223 Regression coverage | #222, #223, #243, #246 | Pending |
| #232 Documentation | #196, #209, #224, #231, #232, #234, #235, #236 | Pending |

## Authentication evidence

- **#157:** Reproduced on the starting revision: the 120/minute socket-peer
  bucket rejected an invalid request after only 119 failures because the valid
  client's earlier request had used the same proxy bucket. Failure budgets now
  use trusted client addresses and separate storage from authenticated work.
  `TestAgentInvalidFloodDoesNotThrottleValidKeys` covers source/global floods,
  independent forwarded clients and recovery with a valid key from a flooded
  source. Lookup concurrency remains bounded; ingress flood protection still
  matters.
- **#187:** Confirmed different results for bare duplicate leaves and full paths.
  Leaf mode now deliberately maps same-named leaves to one policy on every path;
  path mode preserves distinctions. `TestGroupNames` covers both claim forms;
  `TestGroupSyncNamesNestedGroupsConsistently` covers Keycloak memberships. The
  administration guide documents the policy choice and recommends path mode
  when same-named groups need distinct access.
- **#198:** Confirmed discovery and JWKS bodies reached upstream unbounded reads.
  The shared OIDC transport now caps metadata/token responses at 1 MiB, including
  chunked/decompressed bodies. `TestIdentityResponseSizeBound` uses an endless
  reader to prove at most the cap plus one byte is consumed, and accepts an
  exact-cap body. Oversized discovery and JWKS refresh are exercised through
  go-oidc; normal signing-key rotation and cached keys remain functional.
- **#253:** Confirmed absent access-token claims became empty authoritative
  attributes. Claim presence now controls updates; omitted attributes preserve
  stored identity and memberships. Explicit empty groups still remove grants;
  older tokens still cannot restore removed groups. A changed email cannot
  inherit verification. `TestAccessTokenMissingClaimsPreserveStoredIdentity`
  covers a fresh slim token, retained API-key access and explicit revocation.

- **#172:** Added an admin user-list preflight, explicit enabled-state validation
  and a configurable `sync_max_disable` guard (default 10) before any account
  writes. `TestGroupSyncAdminOutageCannotDisableUsers` covers provider-wide 404,
  401 and 503 responses. `TestGroupSyncDisableLimitStagesAllChanges` proves an
  excessive disable count also prevents staged group changes; confirmed bulk
  departures can proceed after explicitly raising the guard. Existing genuine
  user-deletion and API-key invalidation coverage remains in place.
- **#252:** Per-user HTTP failures no longer end the scan. Failed/deferred counts
  and safe causes appear in logs. The syncer rotates from its last attempted
  user and reserves deadline time for completed changes. Integration tests cover
  a persistent first-user 500 and a deadline-limited slow first user, while
  subsequent users still lose or update access. Provider-wide authorization
  and transport failures return before any staged account changes are written.

- **#161:** Local sign-in now remembers up to eight devices per account for
  30 days. Device cookies grant only a separate ten-attempt guessing budget;
  passwords remain mandatory. Anonymous budget storage and password-work
  capacity cannot consume the known-device allowance. Operator recovery can
  write a private curl header file without clearing anonymous throttles.
  Integration coverage keeps an anonymous username/global/table flood active,
  verifies known/new recovery devices can sign in, enforces device guessing
  limits and account/expiry binding, proves cookie-only requests cannot
  authenticate, and checks the registration cap and retention. The CLI recovery
  acceptance test uses a mode-0600 header, completes the required password
  change during a flood and verifies secrets stay out of output and logs.

## Accounting evidence

- **#165:** Confirmed local rejections entered the shared durable queue. They
  now remain in aggregate metrics and metadata request logs, with no per-call
  database cost. Unit tests exercise malformed, unavailable and forbidden
  requests; the PostgreSQL flood regression sends thousands of rejected calls
  while all 50 valid calls retain their request rows and 500 daily tokens,
  without failed or dropped writes.
- **#168:** Confirmed the owner foreign key cascaded end-user history. Migration
  0020 makes forwarding provenance nullable; a database trigger removes direct
  calls during erasure and the existing end-user cascade still removes forwarded
  calls for erased users. Tests cover service erasure, late forwarded completions,
  direct/end-user erasure, portal history and `/v1/me/usage` timings and totals.
- **#178:** Confirmed a missing key caused an in-flight completion transaction
  to fail. The writer now locks surviving users and keys through commit, detaches
  deleted keys and skips erased end users. A blocked real backend call is completed
  after key removal; exactly one keyless row and daily increment survive, with no
  failed writes. A fresh quota manager reloads its daily and monthly charge.
- **#204:** Confirmed deferred cleanup flushed OTLP before accounting. A single
  shutdown coordinator now flushes accounting first (5 s), content traces second
  (2 s) and OTLP last (5 s). A hung-telemetry test verifies order and the independent
  accounting deadline. A real router with a queued database write and an
  unresponsive HTTP collector persists its request and daily charge within the
  accounting budget, then exits after the collector timeout. Existing process
  restart coverage verifies durable charges.
  Deployment budgets are 60 s versus the default 30 s HTTP grace plus 12 s flushes.

## Trace sanitization evidence

- **#216:** A bounded upstream usage observer feeds numeric provider metadata to
  the worker before client filtering and independently of output capture limits.
  Tests retain cached/reasoning usage when clients omit stream usage, including
  a 3,000-token response followed by a tool call. Cut output has a null complete
  tool-call count and unknown finish reason, with observed counts as lower bounds;
  an input-only limit preserves known output facts. Migration 0023 updates limited
  historical list summaries conservatively. History metrics, run labels, filters
  and the detail panel represent unknown counts explicitly. PostgreSQL/proxy tests
  verify unchanged complete client output and separately persisted terminal usage;
  unit tests also cover Anthropic's separate usage events, byte-split Responses
  events, metadata-buffer bounds and the latest provider totals.

- **#214:** An actual pre-0014 fixture reproduces erasure-only implicit consent.
  Migration 0022 requires a new choice for all enabled pre-provenance preferences,
  preserves retention and leaves historical consent dates unknown. A dedicated
  timestamp and transactional `trace.preferences` event record each enabled or
  retention change; repeated saves and erasure do not fabricate new choices.
  A database failure inserting history rolls back the preference too. The API
  returns the timestamp and the settings panel displays the last change.
- **#233:** The portal API example shows `available:false`, `enabled:false` and
  `updated_at:null`, explains both gates, and requires clients to ignore additive
  unknown fields in key, usage and current-user responses. The reference no longer
  calls capture default-enabled, and migration documentation states the actual rule.

- **#166:** Capture begins only after request/model/user/backend/replica admission.
  A PostgreSQL regression floods 400/403/404 and 429 rejections without creating
  traces or consuming the admitted call's capture budget. Per-user process limits
  are four active/queued captures and 30 captures/minute, within a bounded map and
  global 32-slot budget. Retained storage is limited to 512 traces and 64 MiB of
  logical JSON per user across replicas. Separate transaction statements after a
  user-row lock avoid stale snapshots when concurrent writers wait. Generated
  byte sizes cover historical rows; deletion naturally frees budget. Parameters
  have a final 32 KiB aggregate limit, checked again after redaction can expand
  values. Omitted extensions are counted, with ordinary model controls retained.

- **#182:** Stream text is reconstructed before heuristic/operator redaction.
  Version-3 timeline events keep timing/types; raw frames keep protocol structure
  and numeric metadata while omitting free-text fragments. Tests split bearer
  credentials and an operator regex at every byte across OpenAI, Anthropic and
  Responses streams. Structured credential values disappear from parameters,
  raw request/response previews and JSON tool arguments; arguments remain valid
  JSON and omit embedded binary/data-URI values. Historical traces receive the
  current policy on reads without changing their retention.
- **#215:** The final storage sanitizer replaces NUL in JSON keys and string
  values, including JSON-encoded arguments. A PostgreSQL/portal regression stores
  and renders input/output NUL safely with no trace write failure.
- **#251:** Nested tool results have a 32-level bound and avoid repeated recursive
  payload normalization. Tests retain ordinary levels 1–3 and omit level 1500.
  A 1500-level benchmark decoded 150 KiB in 1.19 ms and 608 KiB in 1.65 ms on
  the local Apple M5 Max; work stays bounded as the payload grows.
- **#260:** Preview decoding and final redaction use `json.Number`, preserving
  1234567890123456789 in parameters, nested values, tool JSON and raw previews.
  Only usage statistics intentionally convert numbers for numeric aggregation.
  The portal integration verifies exact values survive PostgreSQL. Browser parsing
  retains unsafe integers as bigints; tree rendering, clipboard and downloads
  preserve numeric JSON tokens. Playwright verifies parameters and all exports.

## Preview lifecycle and credential erasure evidence

- **#169:** Persisted previews retain static operation descriptions and validated
  parameter/field names, without before/after values. Authorized callers still
  receive useful immediate previews. Migration 0025 removes historical snapshots.
  Startup/daily retention deletes expired unused previews and completed/interrupted
  changes older than one day in bounded batches, using partial expiry indexes.
  Privacy and agent guides document content and cleanup lag. A PostgreSQL regression
  prepares a user-override preview containing names, email, a key label and old/new
  reasons, verifies none are stored before or after target-user erasure, and checks
  pruning while fresh previews/results survive. An upgrade test removes old PII.
- **#170:** Migration 0024 makes administration credentials cascade with their owner,
  delegated descendants with their parent, and previews with their credential.
  The actual CLI erases an owner with active, revoked, expired, child and grandchild
  credentials and previews, preserves another owner's credential/preview, and
  retains all `admin_key.*` and `agent.*` audit events. Audit tables remain guarded
  and append-only; target-user preview privacy is handled by minimization rather
  than claiming those target references cascade.

## Administration history filtering evidence

- **#213:** CLI timestamp parsing handles both bounds independently, including
  identical strings. The real binary and portal select exactly the event at that
  instant. History SQL includes only requested predicates and compares actor UUIDs
  as UUIDs; a target-ID-leading index serves cross-type target filters. EXPLAIN
  regressions over 50,000 rows verify actor and target index scans. Invalid actor
  IDs are validation errors. Request-time policy demotion uses transactional
  revocation/audit, retaining owner/key/reason and propagating storage failures as
  503. Tests inject an UPDATE failure, verify rollback, then confirm one successful
  event despite repeated use. Policy-publication revocations use the same service
  and log lookup/storage failures without secret values.

## Administration attribution minimization evidence

- **#244:** Agent sources are reduced to IPv4 `/24` or IPv6 `/48` before audit
  storage/logging and key last-use updates. Database insert/update guards enforce
  this even for older application writers. Migration 0027 minimizes historical
  database addresses in one transaction, restoring the append-only guard before
  commit. Upgrade tests cover IPv4, IPv6, mapped IPv4 and invalid historical values
  and confirm history still rejects UPDATE/TRUNCATE afterward. Authorization uses
  the original address. Documentation describes the retained prefixes, permanent
  history and separately managed historical logs/exports. Identical SSO claims
  skip mutations and audit/invalidation entirely; concurrent first sign-ins are
  serialized. Portal callback and concurrent-sync tests verify one initial event,
  no state/timestamp changes on identical claims, and a new event/state version on
  changed membership.

## History growth protection evidence

- **#248:** Self-service create/revoke/delete attempts share a bounded 30/minute
  per-user budget with 429/retry guidance before mutation or history writes.
  Missing self-service keys and issuance-policy refusals do not generate permanent
  events; operator attempts keep their audit trail. Source-denied agent credentials
  have a separate five-refusals/minute per-key audit bound, independent of source
  rotation and allowed-source traffic. PostgreSQL HTTP regressions attempt 100
  churn cycles, 100 missing-key removals and 100 rotating denied sources, verifying
  exactly 30 successful key events, zero missing-key events and five denied-source
  events, while other users and allowed sources still work. The Admin Guide
  documents process/replica scope and capacity planning/offline archive policy.

## Data access and departed-account evidence

- **#239:** The privacy notice lists administration history and residual
  attribution, group names in exported spans, and authorized `users:read` agent
  clients/operators as recipients of identity and metadata. Operator export uses
  a streaming, consistent read-only snapshot of account data, credential metadata
  without hashes/secrets, overrides, session/device timestamps, attributed usage,
  daily totals, sanitized traces and related history. Private-file mode is 0600,
  refuses overwrite and removes failed partial exports. Actual-CLI tests verify
  structured JSON encoding preserves quoted/backslash-containing identity fields
  without concatenating untrusted values into JSON. They also verify
  203 forwarded calls plus a direct call in the end user's export, exclusion of
  others' calls from the service owner's export, credential exclusion and safe
  files. Snapshot regression excludes concurrently added requests. Disabled dates
  are tracked independently of identity sync; old unknown dates start at upgrade.
  `user list -disabled -older-than DURATION` supports reviewed departures without
  automatic deletion. CLI tests remove an older candidate and cascaded records,
  retain a fresh account and confirm repeated disabling preserves age while
  re-enabling resets it. Runbook/pilot instructions rehearse export and age review.

## Strict-quota request bounds evidence

- **#164:** Strict mode rejects completion multipliers, array/hosted prompts,
  non-integer output limits, remote/inline media, audio output and hosted tools
  before dispatch. Nested duplicate fields, case-variant controls and more than
  64 levels are rejected. Valid single text requests forward a numeric allowance
  equal to the output reservation, preserving large unknown JSON integers.
  Acceptance interpretation: the effective API-native field is used instead of
  always injecting `max_tokens`. Chat preserves `max_completion_tokens` when
  supplied; Responses uses `max_output_tokens`; Anthropic/legacy Completions use
  `max_tokens`. This avoids breaking reasoning models: the
  [official Chat reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)
  documents that deprecated `max_tokens` is incompatible with o-series models.
  Unit and real PostgreSQL/proxy regressions verify invalid shapes never reach a
  backend, forwarded limits match in-flight reservations, daily used tokens stay
  within the quota and reservations drain. Existing concurrent strict, Responses
  and OpenWebUI timing quota regressions remain green.

## Calendar-quota SDK retry evidence

- **#176:** Daily/monthly 429s carry `X-Should-Retry: false` without changing
  Retry-After or reset metadata. Concurrency, RPM, TPM and 503 responses retain
  their retry behavior. HTTP contract tests cover both protocol envelopes and all
  gates. An actual Anthropic SDK 2.8.0 check omits `max_retries`, observes outgoing
  attempts and requires one prompt `RateLimitError`; a subprocess deadline catches
  long reset sleeps. Local PostgreSQL/SDK regression and the mandatory CI
  `make sdk-limits` flow exercise this behavior. The latter first verifies RPM
  remains retryable, then applies a disposable daily limit and erases its fixture.

## Explicit override values evidence

- **#184:** The administrator form accepts only positive digit-only integers
  within the field's safe range. Blank values, separators, suffixes, fractions,
  negatives and overflow show an associated inline error, focus the value field
  and send no mutation. Unlimited requires an explicit selection; strict quota
  uses Enabled/Disabled controls. The mutation receives a validated number or an
  explicitly selected null, never a NaN converted to null. Playwright exercises
  the actual form with invalid inputs, a valid 1000 limit, explicit unlimited,
  concurrency/RPM range overflow and strict-quota disabled. It asserts payloads,
  absence of invalid requests and no browser exceptions. Desktop/mobile visual
  checks cover control alignment and wrapping.

## Batch override inheritance evidence

- **#189:** A finite per-user concurrency override caps the group's batch value,
  including an unlimited value. A valid explicit batch override remains authoritative;
  invalid overrides cannot bypass inheritance. Unit cases cover finite, unlimited,
  smaller and explicit batch values without mutating the base policy. PostgreSQL
  regressions hold interactive and batch calls in flight and verify each class
  rejects its second call for both larger and unlimited group batch limits. The
  managed policy portal preview reports the same inherited cap and explicit override.

## Strict reservation feedback evidence

- **#203:** Calendar refusals distinguish actual exhaustion, a request larger than
  the remaining allowance, and temporary in-flight reservations. Temporary holds
  use a one-second retry and remain SDK-retryable; an oversized request names the
  remaining tokens and suggests reducing its size. A permanent monthly refusal
  wins over a temporary daily hold. Race-tested manager cases cover both calendar
  budgets, exact-fit admission after release, exhaustion, and reservation cleanup.
  Both HTTP envelopes expose the reason and remaining amount with the matching
  retry header behavior. Existing strict-concurrency and default SDK regressions
  exercise the surrounding admission path.

## Unlimited TPM transition evidence

- **#249:** Leases record whether admission used a finite TPM. Unlimited calls
  cannot debit either an old or restored bucket. Admission and usage lookup reset
  the old bucket on an unlimited policy. Outstanding finite-policy completions
  still settle: debt incurred while the bucket is disabled is carried into its
  next finite interval. Race-tested regressions cover the reported hour of heavy
  unlimited usage, restoration with a full bucket, snapshot-only transitions,
  unlimited calls completing late, and finite calls completing both during and
  after the unlimited interval. Existing finite-bucket debt/refill checks remain.
