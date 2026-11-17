# Change and upgrade notes

## Unreleased — audit remediation on main

These notes describe changes on `main`; they do not announce a published release.
The source of detailed evidence is the [remediation ledger](docs/AUDIT_REMEDIATION.md).

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
