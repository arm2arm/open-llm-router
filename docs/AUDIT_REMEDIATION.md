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
| #165 Usage accounting | #165, #168, #178, #204 | Implementation and regressions in this change |
| #166 Trace privacy and bounds | #166, #182, #214, #215, #216, #233, #251, #260 | Pending |
| #169 History and user erasure | #169, #170, #213, #239, #244, #248 | Pending |
| #164 Quotas and admission | #164, #176, #184, #189, #203, #249 | Pending |
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
