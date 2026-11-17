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
| #172 Authentication and identity sync | #157, #161, #172, #187, #198, #252, #253 | #157, #187, #198 and #253 implemented; remaining findings pending |
| #165 Usage accounting | #165, #168, #178, #204 | Pending |
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
