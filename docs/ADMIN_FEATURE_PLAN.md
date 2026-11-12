# Admin capability audit and implementation plan

Audit baseline: commit `6cedb62`, updated for issues #72–#75 and the endpoint editor,
12 November 2026. The remaining delivery stages below are proposals.
Current setup and operation are documented in [ADMIN_GUIDE.md](ADMIN_GUIDE.md).

## GitHub delivery tracking

Implementation is tracked in
[#71](https://github.com/arm2arm/open-llm-router/issues/71), with native sub-issues
and blocked-by dependencies. The milestone order expresses delivery dependencies;
no due dates or assignees have been set.

| Order | Milestone | Implementation issues |
|---|---|---|
| 1 | [M7 · Protected local administration](https://github.com/arm2arm/open-llm-router/milestone/9) | [#72](https://github.com/arm2arm/open-llm-router/issues/72) protected credentials/bootstrap; [#73](https://github.com/arm2arm/open-llm-router/issues/73) independent local authentication; [#74](https://github.com/arm2arm/open-llm-router/issues/74) dual-login UI; [#75](https://github.com/arm2arm/open-llm-router/issues/75) local users/keys; [#76](https://github.com/arm2arm/open-llm-router/issues/76) recovery CLI |
| 2 | [M8 · Admin-managed Keycloak configuration](https://github.com/arm2arm/open-llm-router/milestone/10) | [#77](https://github.com/arm2arm/open-llm-router/issues/77) revisions/secrets; [#78](https://github.com/arm2arm/open-llm-router/issues/78) authentication settings; [#79](https://github.com/arm2arm/open-llm-router/issues/79) SSO activation/rollback; [#80](https://github.com/arm2arm/open-llm-router/issues/80) authorization freshness/group naming |
| 3 | [M9 · Admin-managed endpoints and policies](https://github.com/arm2arm/open-llm-router/milestone/11) | [#81](https://github.com/arm2arm/open-llm-router/issues/81) routing API/runtime; [#82](https://github.com/arm2arm/open-llm-router/issues/82) endpoint/model UI; [#83](https://github.com/arm2arm/open-llm-router/issues/83) group-policy editor; [#84](https://github.com/arm2arm/open-llm-router/issues/84) administration history; [#85](https://github.com/arm2arm/open-llm-router/issues/85) integrated acceptance/docs |

The #72 credential/bootstrap foundation, #73 local authentication APIs, #74
browser sign-in/password flows, #75 local user/key administration, #76
protected-administrator recovery, the #77 managed-settings foundation, the #78
Admin → Authentication page, #79 live activation/rollback, #80 authorization
freshness, the #81 routing revision API and the #82 Admin → Routing page are
implemented in this checkout. Endpoint editing for existing logical models now implements part
of #81/#82: persisted revisions, validation/checks, live activation and restore.
Those issues remain open for logical model/alias creation and the remaining
acceptance work.
Explicit cloud-provider adapters in delivery stage 4 below remain
a future extension; these milestones cover existing compatible backends.
Stage 5's authorization/audit/acceptance work is included in M8 and M9. The
production pilot #21 remains independent.

## Required administrator experience

An operator can install the application, sign in with a protected local
administrator, configure an external Keycloak realm from Admin, and manage backend
connections, model access and user limits. Local login remains available when
Keycloak is absent or unavailable. When Keycloak is configured, it is the default
user sign-in method, with a visible local-login alternative.

The development environment continues to run the bundled local Keycloak with its
test realm and accounts. Production can use an externally administered Keycloak;
changing that connection must not require rebuilding the application image.

These requirements extend the original v1 scope, which excluded cloud-provider
adapters and loaded model/group configuration only from YAML. They are not
additional evidence that the production pilot in #21 has been completed.

## Implemented functionality and gaps

| Capability | Implemented | Evidence / remaining work |
|---|---|---|
| Local development stack with Keycloak | Yes | `deploy/compose/docker-compose.yml`, imported dev/brokered realms |
| External Keycloak connection | Yes, via YAML/environment and restart | `internal/config`, `internal/oidcclient`, portal OIDC setup |
| Keycloak configuration in Admin | No | Portal routes have no authentication-settings editor/API |
| Local users and API keys | Yes, Admin/API/CLI (#75) | Create password-enabled users, require first change/reset, metadata-only key listing/revoke; CLI API-key users can be given a local password |
| Protected permanent local administrator | Yes (#72) | Migration 0004, transactional bootstrap, Argon2id, required first change, database guards |
| Local password authentication and independent startup | Yes, JSON API (#73) | Migration 0005, restricted first-change sessions, rotation/revocation, throttling and OIDC retry; Keycloak absence/outage does not block local Admin |
| Local sign-in/password forms and dual-login UI | Yes (#74) | Keycloak primary action, direct local route, outage state, required first change, Account password rotation and identity-source labels |
| Endpoint pools, credential references, weights and capacity | Yes, Admin/API or YAML | Live revision activation for existing logical models; server-defined secret references and destination policy |
| Endpoint creation/editing/removal in Admin | Yes for existing logical models | `/admin/endpoints`, connection tests, revision conflict checks, persistence and restore |
| Logical model and alias creation/deletion in Admin | No | Model names/aliases remain YAML-owned; #81/#82 remain open |
| Group model grants and default limits | Yes, YAML plus restart | `config.Group`, `policy.Resolve` |
| Group-policy editor in Admin | No | `/admin/groups` returns usage totals; no policy mutation routes |
| Group membership management | Keycloak externally; local memberships in Admin/API (#75) | Assign existing configured groups; sessions end and keys receive updated access; effective models/limits shown |
| Per-user quota/rate/concurrency overrides | Yes, Admin/API/CLI | Numeric fields plus `strict_quota`, reason/actor/time |
| Enable/disable user | Yes, Admin/API; disable through CLI | Protected local administrator cannot be disabled; UI hides its disable action |
| Key administration | Self-service create/revoke, operator CLI create/revoke, Admin list/revoke/copy | Lists expose metadata only; explicit Admin copy requires encrypted storage configured before issuance; legacy keys need replacement |
| User deletion | Operator CLI | Protected local administrator cannot be erased; database guards cover ordinary SQL/cascades |
| Remote compatible backend | Conditional on current native-protocol contract | Origin-only URL, Bearer token, HTTP-200 health probe; not a cloud-provider integration |
| Provider adapters and provider registry | Partial | Endpoint editor detects OpenAI/Ollama model catalogs; no custom auth headers, base paths, signing, provider registry or shared provider capacity |
| Group/provider-specific or money budgets | No | Current limits are per user across models; group totals are reporting |
| Configuration reload and revision history | Endpoint models only | Migration 0006, append-only endpoint revisions, immutable runtime snapshots; no cross-router activation or general settings/history service |
| Standalone config validation command | No | Parser exists; no CLI exposure; Compose validation does not parse router YAML |
| Multiple router instances with shared limits | No | RPM, TPM, concurrency, queues and affinity are process-local |

Code entry points: [configuration](../internal/config/config.go),
[policy resolution](../internal/policy/policy.go),
[portal routes](../internal/portal/portal.go),
[admin handlers](../internal/portal/admin.go),
[portal mounting/startup](../cmd/olr/main.go),
[operator CLI](../cmd/olr/admin.go),
[user store](../internal/auth/store.go),
[proxy](../internal/proxy/proxy.go),
[health and pool selection](../internal/pool/pool.go), and
[Admin UI](../web/src/pages.tsx).

## Delivery sequence

### 1. Protected local administrator and independent local login

This is the foundation: identity-provider changes must never remove the operator's
ability to administer the application.

Completed for #72: versioned salted credentials, singleton bootstrap, first-change
enforcement and current-password rotation, stable identity, independent admin
authority, and API/CLI/database lifecycle guards. Local tests cover concurrent
bootstrap, prior-schema upgrade, restart and secret non-disclosure.
Completed for #73: local JSON login/password/logout handlers, revision-bound
sessions, fixed-window throttling, always-mounted portal routes and independent
OIDC initialization/retry. Tests cover local-only startup, configured IdP outage,
recovery, session fixation/expiry/revocation, CSRF and existing SSO/API-key flows.
Completed for #74: browser local-only/dual login, first-change and Account password
rotation, identity-source labels and session-specific logout. Browser tests cover
local-only mode, an unreachable IdP, the bundled development Keycloak and denied
Admin access for a regular local user.
Completed for #75: Admin/API local user creation, existing-group membership editing,
temporary password resets, disablement with session deletion, effective access and
metadata-only operator key revocation. Tests cover source/protection guards, shared
cache invalidation, concurrent issuance, SSO regressions and the browser lifecycle.
Completed for #76: `olr admin recover` resets only the protected administrator
from a private file or redirected stdin with database access alone, sets a
temporary must-change credential, ends sessions, revokes keys, refuses reuse of
the current password, never reads bootstrap inputs and emits a redacted audit
event. Tests cover an unreachable IdP, idempotent retry, restart with a retired
bootstrap secret and absence of credentials from argv, output and logs.

- Add a local credential store keyed by user UUID, with a maintained password
  hashing implementation, per-password salt and calibrated work factor. Keep
  local and `(issuer, sub)` identities distinct; never link them by email/username.
- Bootstrap one reserved local administrator with a one-time setup credential
  supplied through a private secret file/environment. No production default password.
  Require password change and consume/retire the bootstrap credential. Restarts
  must not overwrite an existing password or recreate the account under a new UUID.
- Represent the protected administrator explicitly. Application APIs/CLI must
  reject deletion, disablement, removal of admin authority or conversion to an
  SSO-only identity. Add database enforcement for the protected record so generic
  user deletion/cascades cannot bypass the application guard. Database superusers
  and intentional full database destruction remain outside this application guarantee.
- Grant this account admin authority independently of mutable Keycloak/group
  policy. Allow password rotation and session revocation. Provide a host-side
  credential-reset command that works without Keycloak and does not print passwords
  or accept them in process-visible arguments.
- Mount the portal with no OIDC configuration. Add a public sign-in screen with
  a local-login option; show Keycloak as the primary choice when enabled. Local
  administrators must have a direct login route that avoids automatic SSO redirects.
- Reuse server-side sessions, secure cookies, origin checks and logout protections.
  Add login throttling, generic invalid-credential errors and tests for password
  guessing protections without creating a permanent lockout of the protected admin.
- Support additional local users through Admin with create, password reset,
  enable/disable and group assignment, while preserving the protected account rules.

Acceptance: a clean installation with no IdP can be administered; both login
methods work with local Keycloak enabled; the protected administrator remains
usable during an IdP outage and after repeated restarts; API/CLI deletion,
disablement and demotion attempts fail. Ordinary users cannot access Admin.

Primary work: migrations, `internal/auth`, new local-auth handlers,
`internal/portal/session.go`, `internal/portal/portal.go`, `cmd/olr/main.go`,
`cmd/olr/admin.go`, React sign-in and admin user pages, Compose bootstrap secrets.

### 2. Admin-managed Keycloak connection and safe configuration activation

Completed for #77: migration 0010 adds immutable settings revisions, a per-domain
active/draft state with optimistic versions, activation history, AES-256-GCM
stored secrets under `OLR_SETTINGS_ENCRYPTION_KEY` and an append-only
`audit_events` table. `olr admin settings` imports the authentication domain from
YAML/environment without behavior change, saves strictly validated drafts,
activates/rolls back after rechecking, refuses file/environment conflicts and
reports effective sources. Startup applies the active revision; an unappliable
revision turns SSO off without falling back to file values, keeping local login.
The Admin page, connection tests and live client swap remain #78/#79.

Completed for #78: **Admin → Authentication** shows authority, runtime revision,
effective sources, conflicts and the portal client's redirect/logout/origin URLs;
imports, edits drafts (including secret rotation and the primary sign-in option)
and discards them. Connection checks report discovery, logout metadata, portal
client authentication, service-token audience and sync permissions with fixed,
redacted messages; test sign-in verifies the identity token and claims with a
draft's settings without creating users or sessions. Changes need a local admin
sign-in from the last 15 minutes; outbound checks refuse link-local/metadata
destinations and never follow redirects. Activation from Admin is #79.

Completed for #79: Admin activation prepares and validates the replacement
portal OIDC client before changing anything, requires passed checks and test
sign-in for SSO-enabled drafts, then switches the portal client, API-token
verifier (dropping its principal cache) and group sync together and stops the
old ones. Rollback and **Apply** work while a provider is down. Sessions are
checked against the current issuer; an issuer change discards pending SSO logins,
keeps accounts separated and reports previous issuers' accounts. Tests cover
switching between isolated realms, failed activation, rollback and SSO off.

Completed for #80: cached OIDC access-token principals carry the authenticator's
invalidation generation, so disablement, overrides and membership changes reach
the next request on every path. Tokens issued before the latest membership update
can only narrow the stored groups and never rewrite them. Login, API tokens and
group sync share one group-name rule (`group_names: leaf|path`); sync reads
Keycloak paths and refuses ambiguous duplicate leaf names.

Completed for #81: the settings service is generic over domains; the **routing**
domain stores complete revisions of logical models, endpoint pools and aliases.
Drafts are checked against strict schema, group/alias references, credential
reference allowlist and outbound destination policy; checks probe changed
replicas without contacting refused destinations; activation publishes a complete
proxy snapshot after health checks while unchanged replicas keep in-flight leases
and accounting; rollback/apply work while endpoints are down. Per-model endpoint
editing closes once routing is managed. `olr config check` validates files
without database or network access.

Completed for #82: **Admin → Routing** shows group → model → endpoint mapping with
health and in-flight counts, imports routing, edits models, replicas and aliases
as drafts with dependency-aware removal, lists validation problems, tests changed
endpoints, activates, discards, rolls back and applies. A browser journey adds a
model and alias on a second compatible backend, verifies a routed completion and
rolls back.

- Add **Admin → Authentication** for SSO enabled/default state, issuer, portal
  client ID/secret, API audience, group/username claim names, optional required
  claim, sync client ID/secret and interval. Derive callback/logout URLs from the
  configured public origin and display them for the Keycloak operator.
- The application configures its connection; realm/client creation, upstream
  brokering and group assignment in Keycloak remain external administration unless
  separately scoped. Test connection must distinguish portal discovery, login,
  token audience and sync permissions.
- Store revisions of mutable settings in PostgreSQL. Import YAML/environment
  settings as an explicit initial baseline; show effective values and their source.
  Choose one active database revision for managed fields and reject conflicting
  overrides rather than silently allowing environment values to defeat UI changes.
  Keep database/bootstrap secrets and listener deployment settings outside the UI.
- Keep secrets in a secret manager or encrypted store with its encryption key
  supplied outside the database. APIs return only configured status/reference and
  rotation controls; never return plaintext secrets or write them to logs/audit diffs.
- Provide save-as-draft, connectivity validation, an explicit test login,
  activation and rollback. Build/test replacement OIDC clients before swapping an
  immutable runtime snapshot; manage closing clients and background sync jobs.
  Database save alone must not claim the active runtime changed successfully.
- A failing external IdP must not stop local portal startup or local login. Report
  SSO health separately, refuse affected SSO operations, and retain local admin access.
- An issuer change creates a new identity namespace; never automatically attach
  old users or keys by matching email. Invalidate affected SSO login challenges,
  sessions and token caches; document disposition of existing keys/users.
- Treat authentication-settings updates as privileged operations requiring recent
  local authentication. Audit actor, timestamp, revision, result and rollback.
- Keep bundled test Keycloak as the dev default. Allow an admin to test/activate
  another realm and restore the local baseline without removing the local login.

Acceptance: a local admin changes from bundled to an isolated external test realm
through the UI without rebuilding the image; the new SSO login works; wrong
secrets, failed sync permissions and unreachable issuers give useful results without
lockout; settings survive restart; rollback restores login; no secret is exposed.

Primary work: revision/secret storage, configuration service, OIDC lifecycle and
sync management, admin settings API/UI, compose configuration, integration tests.

### 3. Admin-managed endpoints, logical models and group policies

Implemented subset: Admin replica add/edit/remove, upstream model/protocol/timeout/
affinity editing, server-side credential references and network guards, health
checks, optimistic revisions, persistence and restore. Runtime activation keeps
existing streams, per-origin capacity reservations and user admission/accounting.
Unit, PostgreSQL integration and browser tests cover these paths. Remaining work
below includes logical model/alias creation, group policies, broader administration
history and CLI validation; this subset does not complete M8 or M9.

- Reuse the revision/activation service for endpoint origin, credential reference,
  health path, weight, capacity; logical model upstream name/protocol/timeouts;
  aliases; and group grants/default limits/admin flag.
- Initially preserve the existing model/pool schema. Add named connections if
  shared provider secrets/capacities are introduced; keep relationships explicit:
  group → model → connection/pool. Do not present an arbitrary replica as a
  group-specific permission when current authorization is model-based.
- Add create/edit/remove forms, connection checks, dependency checks and an
  effective-policy preview for one or several memberships plus user overrides.
  Preview unlimited values explicitly and explain permissive multi-group merging.
- Use revision checks for concurrent admin edits. Prevent deleting models still
  referenced by groups/aliases, and identify affected users before activation.
- Swap model/policy runtime snapshots safely; allow old in-flight requests to
  drain and do not reset active concurrency/accounting state or bypass quotas on
  an edit. Rollback must restore a complete revision, not partial field values.
- Validate server-side destination changes and redirects; block access to local
  metadata/control services while allowing explicitly configured private GPU
  networks. Secret-bearing connection tests must follow the same destination policy.
- Add operator key-revoke and local-user group editing to Admin. Keep SSO
  membership read-only in this UI unless an explicit Keycloak write integration
  is introduced; labels must identify the authoritative identity source.
- Expose the existing strict parser as a read-only `olr config check` command for
  file-managed deployments, without database migrations or starting the server.

Acceptance: an admin adds a compatible backend, creates a model, grants it to one
group and sets that group's limits; authorized users list/use it and other users
cannot. Changing a user's override applies without restarting. Invalid drafts
never replace active settings; rollback, concurrent edits, draining streams and
secret redaction are verified.

Primary work: configuration store/service/API, `internal/config`, pool lifecycle,
policy consumers, admin forms, CLI validation and existing proxy/policy tests.

### 4. Explicit external LLM provider support

This extends the original locally hosted backend contract. Support should be
declared per tested provider and operation rather than inferred from an HTTPS URL.

- Define connection types: current native compatible backend first, then named
  providers. Add base-path/deployment URL rules, authentication strategies,
  server-controlled headers, health strategy and provider-specific TLS where needed.
- Keep provider credentials distinct from caller credentials. Preserve destination
  validation, strip caller auth, redact secret values and test header isolation.
- Preserve native protocol behavior. If translation is later desired, scope it
  separately with tool/stream/cancellation/usage tests rather than implying that
  one protocol flag supplies translation.
- Document supported models/routes, service limits, cancellation semantics and
  usage completeness. Add shared connection capacity/rate handling when one
  provider serves several logical models. Avoid retrying committed output.
- Grant providers through explicit logical-model mappings to groups. If different
  groups need different providers for the same apparent task, use separate models
  first; any group-dependent routing needs an explicit policy design.
- Scope provider-specific/group-wide token budgets and monetary budgets separately;
  existing per-user quotas cannot serve as a shared provider cost ceiling.

Acceptance: each supported provider has direct-versus-routed text, streaming,
tools, cancellation and usage checks for its advertised protocols; unauthorized
groups are denied; timeout/rate-limit responses are handled; credentials never
reach clients. Do not enable a provider based solely on a successful health probe.

### 5. Harden policy freshness and operations

- Unify invalidation of API-key and OIDC token principals for user disablement,
  override changes and group changes. The current OIDC verifier caches a full
  principal for up to one minute; document and test the stronger promised behavior
  before claiming next-request changes for every authentication method.
- Align login/access-token group naming with Keycloak sync. Sync currently reads
  leaf names, so full-path and nested-group support needs explicit tests/design.
- Add append-only administration events for settings, overrides, account changes
  and credential rotation, excluding secrets. The current override row records
  only the most recent change.
- Verify restore/upgrade preserves protected admin identity, credential rotation
  and active configuration revision. Test local access during an OIDC outage.
- Keep multi-router deployments out of the supported configuration until shared
  admission counters/queues and consistent revision activation are implemented.

## Completion record

Each stage should land with migrations and rollback/upgrade notes where relevant,
an updated [admin guide](ADMIN_GUIDE.md), authorization and failure-path tests, and
a runnable local acceptance walkthrough. Mark capabilities implemented only when
the relevant API, UI, persistence and enforcement path are connected and verified.

The existing production-pilot work remains tracked by
[#21](https://github.com/arm2arm/open-llm-router/issues/21): site access, deployed
IdP/GPU acceptance, an unchanged image during the IdP switch, two weeks of real
user activity, and triaged problems. This feature plan neither checks those boxes
nor changes that issue's scope automatically.
