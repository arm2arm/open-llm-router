# Change and upgrade notes

## Unreleased — audit remediation on main

These notes describe changes on `main`; they do not announce a published release.
The source of detailed evidence is the [remediation ledger](docs/AUDIT_REMEDIATION.md).

- Sign-in and Connect can link the operator’s privacy notice. Connect shows the
  possible recipients of a request for each permitted model and alias, including
  unlabelled destinations. Both portal settings are optional.
- OpenAI streaming preserves supplemental metadata on final usage events.
  Ordinary usage-only events can still be suppressed when the client omits usage.

- Helm upgrades preserve an existing settings encryption key when values omit it
  or supply an empty entry, and reject accidental replacement with another key.
  Ordinary removed backend tokens are still removed from the managed Secret.

- Institutional sign-in has a prominent Keycloak card; local credentials are
  collapsed when institutional login is primary. Direct local recovery stays available.
- Admins can rename public model routes, including generated names, while
  preserving upstream model IDs, replicas, group grants and alias targets.
  New collision names use the backend's readable name instead of its internal ID.
- A public About page brings together project details, developer credits and
  avatars, development tools, funding and citation links.
- Open WebUI Direct Connections can use personal OLR keys through opt-in
  `cors.allowed_origins`. Approved browser preflight is anonymous; inference
  authentication, group limits and streaming cancellation remain enforced.
- Backend cards have a **Clone** action that pre-fills a separate backend for
  review, with a unique name and the original model, group and preset selections.
  Key-protected copies require their own API key entry.
- Advanced routing lets administrators rename a replica's shared backend directly.
  Selectors display the backend name while retaining stable IDs and credentials.
- The standard router image includes Bash, curl and CA certificates for pod-local
  diagnostics. It continues to run as UID 65532 with the existing Helm security context.
- API keys are stored as hashes and shown only at creation. Remove the retired
  `OLR_API_KEY_ENCRYPTION_KEY` setting. A lost key needs replacement and revocation;
  administrators cannot retrieve it. Keep the separate settings encryption key
  when using encrypted managed SSO/backend secrets.
- Protected local administration, local/SSO sign-in, backend discovery, logical
  model aliases, group-policy editing and append-only administration history are
  available in Admin. Changes use revision checks and preserve running streams.
- Admission, quota reservations, protocol parsing, identity freshness and scoped
  administration-key authorization have stricter validation. Review explicit
  finite limits and client retry handling after an upgrade.
- OpenAI Chat Completions and stateless Responses, Anthropic messages, streamed
  usage and throughput metadata have client regression coverage. Individual
  backends must implement each protocol/endpoint you expose.
- `/readyz` checks database availability. Backend availability, capacity and model
  selection are separate; verify Admin service status and a real inference call.
- Personal content tracing is disabled by default and requires both the site's
  tracing gate and the user's explicit opt-in. Apply retention to exports too.
- CI requires its test database setting, scans added secret history, runs security
  mutation checks and tracks pinned image/tool releases. Signed image releases
  include immutable digest references, provenance, license notices and build info.

Before upgrading, export administration history to restricted external storage,
back up PostgreSQL and the managed-settings encryption key, record image/config
versions, and test restoration in isolation. Migrations run at startup. Follow
[backup and restore](docs/RUNBOOK.md#backups-and-restore); restoring old data can
undo later revocations, disablements and policy reductions. Reapply those actions
from the external record before reopening traffic. Do not start an old binary
against an upgraded schema without a validated rollback plan.
