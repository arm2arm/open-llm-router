# Administration delivery record

The administration stages originally proposed on 12 November 2026 are implemented.
Use [ADMIN_GUIDE.md](ADMIN_GUIDE.md) for current workflows,
[PORTAL_API.md](PORTAL_API.md) for the browser API and
[AGENT_ADMINISTRATION.md](AGENT_ADMINISTRATION.md) for scoped REST/MCP automation.
The production pilot remains a site acceptance activity in [PILOT.md](PILOT.md).

## Delivered capabilities

| Capability | Current behavior and evidence |
| --- | --- |
| Protected local administrator | Stable account UUID; one-time bootstrap, required password change, local login and operator recovery independent of Keycloak. See [protected administration](ADMIN_GUIDE.md#bootstrap-the-protected-local-administrator). |
| Local users and API keys | Create/edit local users, group assignments, enablement, password resets, finite key lifetime/cap and one-time key display. Existing keys cannot be recovered. |
| Admin-managed authentication | Validate/test encrypted SSO settings, activate/rollback revisions live, select local/Keycloak default sign-in, normalize groups and enforce issuer separation. |
| Backends workspace | Named backend add/edit/remove, HTTP or HTTPS, optional key, discovery/refresh beside URL, model selection, group visibility and editable limit presets. |
| Logical models and aliases | Managed routing revisions support model/alias creation and deletion, pool parameters, preview, validation, live activation and rollback. Existing streams retain their snapshot. |
| Group-policy editor | Model grants, permissive multi-group limit resolution, strict quota and per-user override preview. Protected recovery authority stays independent of mutable groups. |
| Administration history | Append-only actor/target/result events for credential, account, routing and policy changes; searchable Admin page and `olr admin history` exports. Recovery also emits a redacted CLI event. |
| Scoped administration API | Expiring source-restricted keys, narrowed delegation, REST/MCP operation discovery and preview/apply binding. See [AGENT_ADMINISTRATION.md](AGENT_ADMINISTRATION.md). |
| Validation and regression checks | `olr config check`, database/race integration, browser/client journeys, 44 security mutations, Helm/network checks and release policy in [CI](../.github/workflows/ci.yml). |

The historical delivery was tracked in
[#71](https://github.com/arm2arm/open-llm-router/issues/71) and its administration
milestones, followed by the backend workspace
[#266](https://github.com/arm2arm/open-llm-router/issues/266).
The audit findings are consolidated and evidenced in
[AUDIT_REMEDIATION.md](AUDIT_REMEDIATION.md).

## Decisions retained in the implementation

- Group administrators and protected recovery have separate authority. The
  protected local administrator also has all inference models and unlimited
  limits; its keys must be treated as privileged credentials.
- Local and `(issuer, sub)` identities remain distinct. Email and username never
  silently merge accounts across issuers. Identity-provider outages leave local
  recovery and API-key inference usable while database access remains available.
- API keys are hashing-only. Managed SSO/backend secret ciphertext uses the
  separate settings encryption key; no UI retrieves existing inference keys.
- Backend URLs may use HTTP on a trusted internal network, with or without a
  backend key. Cluster routing, egress policy and firewalls must allow the router
  pod to reach them. Browser reachability alone is insufficient.
- Mutations use version checks and history. Site policy may require independent
  review, but the application and repository do not impose that release gate by
  default. [RELEASE_SECURITY.md](RELEASE_SECURITY.md) describes the current policy.
- Content tracing requires both site approval and each user's explicit opt-in.
  Metadata accounting remains separate from content retention and capture failures.

## Remaining platform extensions

Shared cross-process admission/quota counters and coordinated multi-router runtime
activation are not implemented. Use one router process: RPM, TPM, concurrency,
queues, affinity and daily/monthly quota reservations are process-local; multiple
processes can multiply the effective allowance. Database-persisted usage supports
reporting, but it does not make these admission limits distributed.

Cloud-provider-specific adapters, group-wide or monetary budgets and broader
multi-instance coordination remain potential extensions. Current routing forwards
only the supported OpenAI/Anthropic protocols to compatible backends; the individual
backend must implement the endpoints selected for its model. New deployment claims
need their own acceptance evidence rather than the historical plan's status.
