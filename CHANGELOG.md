# Change and upgrade notes

## Unreleased — audit remediation on main

These notes describe changes on `main`; they do not announce a published release.
The source of detailed evidence is the [remediation ledger](docs/AUDIT_REMEDIATION.md).

- **Admin → Advanced routing** opens on a searchable route list. Create or edit one
  route at a time, review a saved draft and its effect on users, then activate
  it. Aliases, group access and revision history have dedicated views.

- Administration key rotation now identifies a network or permission outside
  the original key's grants. The wizard prompts for sign-in when the server
  requires fresh authentication.

- `GET /v1/me/usage` exposes the calling API key's label, workload class and
  expiry alongside its ID and display prefix. Clients that use a pasted key
  can warn the user before it expires.

- A trusted application that signs users in through the router's Keycloak realm
  can issue each user an inference key, without sending them to the portal. The
  new administration-key permission `user_keys:issue` (grantable alone; it does
  not include `users:read`) adds `issue_user_key`, `revoke_issued_key` and
  `get_issued_key_summary`. Issuance works only for the configured OIDC issuer,
  never for local accounts or the protected administrator. A user who has never
  signed in is recorded from Keycloak when the group-sync service account is
  configured. Disabled users and users without model access get no key. Issued
  keys carry a label prefix, respect `auth.max_key_lifetime_days` and
  `auth.max_active_keys_per_user`, and appear in the user's portal key list.
  Only the issuing administration key can revoke or summarize them through the
  API. Each issue and revocation is in history, without the key. The optional
  `admin_api.user_key_issuance` setting limits issuance by group and lifetime and
  sets the label prefix. The Agent access wizard marks the permission as
  sensitive and warns about it. **Upgrade:** migration 0036 adds the issuing
  administration key to inference keys; existing keys are unchanged.

- `GET /v1/models`, the portal's `/portal/api/models` and the `issue_user_key` and
  `get_issued_key_summary` responses report each model's
  `context_length`, `type` (`chat` or `embedding`) and `protocols`, so agents can
  size their context and portals never pick an embedding model as a chat
  default. Aliases report their target's values. Models accept two optional
  settings: `context_length` (a positive integer) and `type` (default `chat`).
  Without a configured `context_length`, health checks read it from the
  replicas' `/v1/models` catalogs (vLLM's and SGLang's `max_model_len`) on the
  first successful check, on recovery and every 10 minutes, and report the
  smallest value across the replicas; the field is omitted while unknown.
  Backends that serve no catalog, such as Ollama, see one extra
  `GET /v1/models` per replica every 10 minutes. Endpoint discovery shows the
  reported values, and the routing editor can set both settings. Existing
  configurations and clients work unchanged. mock-llm reports `max_model_len`
  (`MOCK_MAX_MODEL_LEN`, default 32768) and counts catalog reads apart from
  forwarded requests in `/_mock/stats` (`catalog_requests`).
- Admin-added backends can no longer reach blocked destinations through IPv6
  forms that a translator delivers to IPv4: NAT64 (`64:ff9b::/96`) and
  IPv4-compatible addresses get the verdict of their embedded IPv4 address,
  local-use NAT64 needs an explicit `OLR_BACKEND_ALLOWED_CIDRS` grant, and
  IPv6 zone identifiers are refused. A zone previously let a private-network
  origin evade the AWS IPv6 metadata block. Grant translated private
  backends by their IPv4 network. Identity-provider checks apply the same
  rules to metadata addresses and now also refuse Alibaba's `100.100.100.200`.
- Queued inference requests cost far less CPU: a release no longer makes every
  caller queued for any model rescan the pool.
- Forwarding large requests is cheaper: routing fields are read, and the
  model, priority, `stream_options` and `store` fields rewritten, without
  decoding and re-encoding the body. A 0.8 MB chat request takes 2.3x less
  CPU and a tenth of the memory; a body of many small fields no longer
  allocates per field. Other members are forwarded byte for byte (no key
  re-sorting or HTML escaping). When a model sets `priority`, the router also
  removes case variants such as `"Priority"`, which some decoders apply.
- The portal loads faster: its content-hashed JavaScript and CSS bundles are
  cached by the browser and sent gzip-compressed (about 705 KB down to 200 KB),
  instead of being transferred again, uncompressed, on every page load.

- The API-key creation dialog explains Interactive and Batch use, their separate
  concurrency limits and shared quotas, and labels Interactive as the default
  for most uses.
- Backend editors can publish an organization name and recipient privacy URL.
  Connect explains AI interaction, retries and backend processing, shows per-model
  recipient privacy links, and identifies missing notices. Authenticated model
  catalogues add `recipient_details` and `recipients_complete`; existing portal
  recipient labels and privacy configuration remain supported.
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
