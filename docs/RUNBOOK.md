# Operations runbook

Practical installation and upgrade commands for Docker Compose and Kubernetes/Helm
are in [INSTALL.md](INSTALL.md). The [Helm chart](../charts/open-llm-router) is the
packaged Kubernetes installation path; Kustomize remains available below.

Everything an operator needs to deploy and run the router. Behaviour in detail is in
[REFERENCE.md](REFERENCE.md); the portal API in [PORTAL_API.md](PORTAL_API.md); metrics and
traces in [OBSERVABILITY.md](OBSERVABILITY.md); the privacy notice in [PRIVACY.md](PRIVACY.md).
For a setup walkthrough and the current administrator capabilities, start with
[ADMIN_GUIDE.md](ADMIN_GUIDE.md). Missing local-login and Admin configuration features
are tracked in [ADMIN_FEATURE_PLAN.md](ADMIN_FEATURE_PLAN.md).

## Deployment

The router is one container image (`olr`, distroless, non-root, port 8080) plus PostgreSQL 17
and an OIDC provider (Keycloak). The image carries no configuration: mount the site's config
file at `/etc/olr/config.yaml` and pass secrets in the environment. A private deployment file
holds the site's values for the keys of [config.example.yaml](../config.example.yaml); it does
not belong in this repository. Run one router instance (see [Known limitations](#known-limitations)).

The standalone [production Compose template](../deploy/compose/pilot.yml), private site
configuration examples, onboarding checks and two-week closure record are in
[PILOT.md](PILOT.md). Use that file for a production realm and real GPU pools; the root
Compose stack includes development Keycloak and mock backends.

Startup applies database migrations and fails before listening if the database is unavailable.
OIDC discovery, token verification initialization and group-sync initialization run separately
and retry after failures; local authentication and API keys remain usable. `/portal/api/auth`
reports SSO availability. `/healthz` is liveness; `/readyz` needs the database and one healthy replica
per model. A first `SIGTERM` gives streams 30 s (`-shutdown-grace`) to finish.

### Behind a TLS reverse proxy

Terminate TLS in a reverse proxy and keep the router's port internal. The proxy must stream:

- response buffering **off** (the router also sends `X-Accel-Buffering: no`);
- read/send timeouts **at least** the largest model `timeout_s` (default 900 s);
- request body limit **4 MiB**, the router's own limit;
- HTTP/1.1 to the router, keep-alive on; `/metrics` not exposed publicly, or only with its token;
- per-client connection and request-rate limits, with a bounded upload timeout;
  the router does not trust `X-Forwarded-For`.

```nginx
# http context
limit_conn_zone $binary_remote_addr zone=olr_clients:10m;
limit_req_zone $binary_remote_addr zone=olr_requests:10m rate=10r/s;
server {
    listen 443 ssl;
    server_name llm.example.org;
    client_max_body_size 4m;
    client_body_timeout 120s;
    limit_conn olr_clients 20;
    limit_conn_status 429;
    limit_req zone=olr_requests burst=20 nodelay;
    limit_req_status 429;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 960s;
        proxy_send_timeout 960s;
    }
    location = /metrics { deny all; }
}
```

The edge timeout bounds gaps between upload reads; the router additionally applies a
two-minute absolute upload deadline. Disable request buffering so that deadline covers
the client upload. Connection limits also count long streams; tune them and rate limits
for shared NATs and your workload. Configure real client IP restoration only from trusted
load balancers; otherwise limit the connecting address. Limits are per edge instance.

For the F5 NGINX controller, merge the data in
[`deploy/nginx/f5-controller-config.yaml`](../deploy/nginx/f5-controller-config.yaml) into
the ConfigMap referenced by its `-nginx-configmaps` argument, and use the chart
production ingress annotations. ConfigMap keys are documented in the
[F5 controller reference](https://docs.nginx.com/nginx-ingress-controller/configuration/global-configuration/configmap-resource/).
For community ingress-nginx, the base Ingress includes connection/rate limits; merge
[`deploy/nginx/ingress-nginx-controller-config.yaml`](../deploy/nginx/ingress-nginx-controller-config.yaml)
into its controller ConfigMap (this is a controller setting,
not an Ingress annotation). Verify the generated NGINX config before public exposure.

Authentication uses two complementary limits: the trusted edge limits the connecting
source, including rotating invalid credentials; the router hashes each presented API
key or token with SHA-256 and throttles repeated failures for that digest. Never use
raw Authorization or X-Api-Key values as proxy bucket keys or put them in access logs.
Keep the router inaccessible from the Internet except through the edge, and strip or
overwrite forwarding headers there. Neither the credential throttle nor the source
limits above use client-supplied forwarding headers. For a load balancer in front of
NGINX, trust only its explicit address ranges when configuring real-IP restoration.

Each router admits at most **eight** concurrent API credential cache misses, including
requests waiting for a shared API-key refresh. API-key and OIDC verification share that
budget. A full budget fails immediately with **503** and `Retry-After: 1`; cached valid
principals remain usable. Credential SQL uses a separate **four-connection** pool, so
invalid credentials cannot occupy the portal/accounting/readiness pool. Allow those
four connections per router in the database's connection budget, plus its dedicated
invalidation connection and the primary pool configured by `pool_max_conns` in
`OLR_DATABASE_URL`. These fixed authentication limits require no configuration.

Failed API keys and tokens occupy separate bounded caches of **4,096 SHA-256 digests**
for **five seconds**. Unknown, expired, revoked, disabled and otherwise invalid
credentials receive the same 401; after ten failures for a cached digest, further
attempts receive **429** with `Retry-After: 5`. Retries never extend the fixed expiry.
Database errors and cancelled work are not negatively cached. Admin invalidation
clears API-key denials and invalidates token denials by generation. A new random API
key is usable immediately; if its exact value was previously presented while absent,
the denial expires within five seconds even when invalidation is unavailable.

JWKS fetches have a **ten-second minimum interval per OIDC client** and at most one
active response, including failed fetches. Unknown `kid` values and bad signatures
cannot reset or extend the interval. Cached signing keys continue working during it;
a newly published signing key succeeds on a retry after the interval. A token tried
during the cooldown may additionally retain a five-second negative-cache entry, so
allow up to **15 seconds** and retry after a signing-key rotation. Publish overlapping
signing keys in advance where the IdP supports it. Portal login and API verification
have separate OIDC clients, so API-token floods cannot consume the portal's JWKS slot.
All in-memory budgets/caches are per router; multiply limits by replicas when sizing
the edge and database.

Run `python3 tests/security/check_edge.py` with Docker available to exercise the
NGINX example against rotating credentials and spoofed forwarding headers. This check
also runs in CI. The Go race/integration tests cover work limits, negative-cache
expiry/invalidation, signing-key rotation and portal access while credential SQL is
stalled. Validate equivalent limits against the actual deployed ingress controller.

Set `PORTAL_BASE_URL=https://llm.example.org` (secure cookies are the default and require
HTTPS). In the identity provider, register the confidential client `olr-portal` with redirect URI
`https://llm.example.org/portal/callback`, post-logout redirect `https://llm.example.org/`,
PKCE S256 and a groups mapper. Switching from the dev Keycloak to a production one is
configuration only: `OIDC_ISSUER`, client ID and secret (and remove `OIDC_BACKCHANNEL_URL`).

Uploads acquire both slots before any body read, including unknown-length/chunked uploads.
Excess user uploads receive 429; a full process upload budget returns 503. Both include
`Retry-After: 1`. Slots remain held until normal admission finishes and are released on all
errors and disconnects. The default pre-admission payload ceiling is 128 MiB globally and
16 MiB per user (4 MiB for concurrency one), plus buffer growth, JSON/tracing copies and
runtime overhead. Admitted requests and backend queues need additional memory headroom.
All three upload settings must be positive and require a restart to change. Increase the
body cap only for a measured workload, reducing reader counts to fit the memory budget.

### Configuration reference

The config file is YAML, read strictly: unknown keys, unset `${VAR}` references and invalid
values stop the start with a list of every problem. Limits of 0 are refused. Omitted group
limits mean unlimited; the process upload bounds above always have finite defaults.

| Key | Default | Meaning |
|---|---|---|
| `auth.default_key_lifetime_days` | 90 | default lifetime, at most the site maximum |
| `auth.max_key_lifetime_days` | 90 | maximum lifetime in every issuance path (1–36500) |
| `auth.max_active_keys_per_user` | 10 | active keys per user (1–1000), enforced atomically |
| `limits.timezone` | UTC | IANA zone of daily/monthly quotas and daily totals |
| `limits.max_body_bytes` | 4194304 (4 MiB) | maximum proxied body size; match the edge cap |
| `limits.max_body_readers` | 32 | simultaneous uploads, parsing and normal admission per process |
| `limits.max_body_readers_per_user` | 4 | upload slots shared across keys/classes, further capped by effective class concurrency |
| `retention.usage_days` / `usage_daily_days` / `run_at` | 90 / 400 / `03:00` | how long records and daily totals are kept; when the nightly job runs |
| `oidc.issuer`, `portal_client_id` | — / `olr-portal` | identity provider; leave `oidc` out for local portal authentication and API keys |
| `oidc.groups_claim`, `username_claim` | `groups`, `preferred_username` | claim names |
| `oidc.audience` | — | enables access tokens on `/v1/*` with this `aud` |
| `oidc.sync_client_id`, `sync_interval_s` | `olr-sync`, 900 | group sync (runs when its secret is set) |
| `oidc.required_claim` | — | `{name, value}` a portal login must carry |
| `models.<name>.protocols` | — | `openai`, `anthropic` or both |
| `models.<name>.upstream_model` | the name | model name sent to the backend |
| `models.<name>.timeout_s` | — | wait for response headers |
| `models.<name>.idle_timeout_s` | 30 | silence allowed between chunks |
| `models.<name>.queue_timeout_s` | 0 | wait for a free replica when all are full |
| `models.<name>.session_affinity` | false | `X-Session-Id` keeps a session on one replica |
| `models.<name>.priority` | — | `{interactive, batch}` sent as vLLM `priority` |
| `models.<name>.pool[]` | — | replicas: `url` (origin), `weight`, `max_inflight`, `health_path` (`/health`), `bearer_token_env` |
| `groups.<name>.models` | — | allowed models, `"*"` for all |
| `groups.<name>.tokens_per_day`, `tokens_per_month`, `rpm`, `tpm` | unlimited | quotas and rates per user |
| `groups.<name>.max_concurrent`, `max_concurrent_batch` | unlimited | calls at once for interactive / batch keys |
| `groups.<name>.strict_quota` | false | reserve `max_tokens` + request size before a call |
| `groups.<name>.priority`, `admin` | 0, false | added to the class priority; portal admin pages |
| `aliases` | — | client model name → configured model |
| `forwarders.header_map`, `unknown_user` | `X-OLR-End-User: username`, `reject` | trusted forwarders |

| Environment variable | Meaning |
|---|---|
| `OLR_CONFIG` | config file path (image: `/etc/olr/config.yaml`) |
| `OLR_DATABASE_URL` | Postgres URL — **secret** |
| `OLR_SETTINGS_ENCRYPTION_KEY` | optional base64 32-byte AES key for stored managed-settings secrets — **secret**; keep stable and back up separately from PostgreSQL |
| `OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE` | owner-only file for one-time protected local administrator bootstrap; preferred over environment password |
| `OLR_BOOTSTRAP_ADMIN_PASSWORD` | alternative bootstrap password — **secret**; configure only one source; ignored once initialized |
| `OIDC_ISSUER`, `OIDC_PORTAL_CLIENT_ID`, `OIDC_GROUPS_CLAIM`, `OIDC_USERNAME_CLAIM` | override the `oidc` keys; an issuer enables SSO; the portal is always mounted. After `olr admin settings import`, the database revision is authoritative and differing values are ignored with an error log |
| `OIDC_PORTAL_CLIENT_SECRET`, `OIDC_SYNC_CLIENT_SECRET` | client secrets — **secret** |
| `OIDC_AUDIENCE`, `OIDC_SYNC_CLIENT_ID`, `OIDC_REQUIRED_CLAIM=name=value` | override the `oidc` keys |
| `OIDC_BACKCHANNEL_URL` | reach the issuer at an internal URL; the issuer stays the public one |
| `PORTAL_BASE_URL`, `PORTAL_COOKIE_SECURE` | public origin; secure cookies (default true) |
| `OLR_METRICS_TOKEN` | bearer token for `/metrics` — **secret**; empty refuses scrapes |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` | OTLP/HTTP traces |
| `OLR_TRACE_USER_ID` | add the user ID to spans (default false) |
| `OLR_BACKEND_ALLOWED_CIDRS` | comma-separated private GPU networks allowed for new Admin endpoints; empty by default (dev Compose: `192.168.0.0/16`) |
| `OLR_BACKEND_TOKEN_ENVS` | comma-separated additional backend-token environment names selectable by Admin; supply values in the process environment too |
| any name in `bearer_token_env` | a backend's API key — **secret** |

### Kubernetes

For new installations, use the [Helm walkthrough](INSTALL.md#kubernetes-with-helm).
Its Ingress is optional and controller annotations are configurable. The older base
and acceptance script below target community ingress-nginx, which
[retired in March 2026](https://kubernetes.io/blog/2026/03/30/kubernetes-v1-36-sneak-peek/);
translate their streaming settings to your maintained ingress controller.

[`deploy/k8s/base`](../deploy/k8s/base) is a kustomize base with the same image and
environment as Compose: a single-replica Deployment (`Recreate`, non-root, read-only root
file system, restricted Pod Security), a Service, and an ingress-nginx Ingress with buffering
off, 960 s read/send timeouts and a 4 MiB body limit. Postgres is external. In the site's
private overlay:

```sh
kubectl -n olr create secret generic olr-secrets \
  --from-literal=OLR_DATABASE_URL='postgres://…' \
  --from-literal=OIDC_PORTAL_CLIENT_SECRET='…' --from-literal=OLR_METRICS_TOKEN='…'
kubectl -n olr create secret tls olr-tls --cert=… --key=…
# overlay: replace config.yaml and router.env, set the host, then
kustomize edit set image olr=registry.example.org/olr:<tag>
kubectl apply -k <overlay>
```

The router migrates the database at startup and restarts until Postgres is reachable; run
`olr migrate` as a Job first if preferred. Keep `replicas: 1` until counters are shared
(see Known limitations). With another ingress controller or a Gateway, set the equivalent of
buffering off and timeouts of at least `timeout_s`. `/metrics` stays behind its token; scrape it
through the Service.

[`deploy/k8s/test`](../deploy/k8s/test) adds an in-cluster Postgres and the two mock backends.
`tests/k8s/acceptance.sh` deploys it into a disposable kind cluster behind ingress-nginx and
runs the Compose acceptance checks through the Ingress: both SDK suites with cancellation
evidence, the rate-limit checks, a 75-second stream with 3-second gaps, and first byte within a
second (no buffering). Portal login is not part of it: the overlay runs without OIDC.

## Everyday operations

### Manage endpoint revisions

Use [Admin → Endpoint configuration](ADMIN_GUIDE.md#configure-endpoints-in-admin)
for live edits to replicas and model parameters of existing logical models. The
active PostgreSQL revision overrides YAML for that model and survives restart.
Restore an older revision or current file configuration from the UI. Health and
metrics use the active pools; old streams drain without resetting user limits.
Keep one router process: there is no cross-process activation broadcast. Back up
`endpoint_revisions` and `endpoint_active` with the rest of the database.

Network allowlists and secret references are operator-owned. Recreate the process
after changing allowed CIDRs, adding a token reference or rotating its environment
value. File-defined destinations and token references remain trusted. Never use
unrelated database/OIDC secrets as backend credentials. Model names, aliases,
group policies, OIDC and other file settings still require recreation.

### Bootstrap, rotate or recover the protected local administrator

Follow [the administrator guide](ADMIN_GUIDE.md#bootstrap-the-protected-local-administrator)
for private secret files and the initial change. `olr admin bootstrap` runs without
Keycloak; `olr serve` also bootstraps when a secret source is configured. There is
no default password. An initialized account ignores bootstrap inputs, including
missing retired files, and preserves its UUID and password on restart/upgrade.

```sh
olr admin bootstrap -password-file /private/secrets/olr-bootstrap
olr admin password change -user USER_UUID \
  -current-password-file /private/secrets/olr-current \
  -password-file /private/secrets/olr-new
```

The first change is required before session/API-key access. Later rotations use
the same command and revoke existing sessions; they require the current password.

If the password is lost, reset it with only database access; Keycloak may be down.
The new password is temporary and must be replaced at the next local login. The
old password, every session and every API key of the account stop working; the
UUID and admin authority are kept. Bootstrap inputs are never read. Each attempt
writes a JSON `admin audit` line to stderr. Details, outputs and retry behaviour
are in [the guide](ADMIN_GUIDE.md#recover-the-protected-local-administrator).

```sh
olr admin recover -password-file /private/secrets/olr-recovery
docker compose exec -T router olr admin recover -password-stdin < /private/secrets/olr-recovery
kubectl -n olr exec -i deployment/olr-open-llm-router -- \
  olr admin recover -password-stdin < /private/secrets/olr-recovery
```

Changing the bootstrap secret after initialization has no effect; retire it after
the first change and use recovery if it was exposed before then. Local JSON login/password/logout
handlers work without Keycloak; [the guide](ADMIN_GUIDE.md#use-local-authentication-without-keycloak)
shows their use. Browser local sign-in is at `/portal/login/local`; the first login
requires a replacement password, and later rotation is under **Account**. The
reserved local administrator cannot be deleted, disabled, demoted or
converted to an SSO identity through ordinary application/SQL operations.

### Manage ordinary local users

Use **Admin → Create local user** for a password-enabled account and assign existing
configured groups. New users must replace their temporary password at first login.
Under **Manage**, edit local memberships, inspect effective models/limits and key
metadata, revoke individual keys, disable/re-enable, or reset the local password.
Resetting permanently revokes every key and ends all sessions; membership changes
end sessions and update existing keys' access. Disabling deletes sessions and blocks
keys; re-enabling requires fresh browser login but restores unrevoked keys.
Keycloak passwords/memberships remain IdP-managed. These actions cannot reset or
reassign the protected administrator. See the
[administrator guide](ADMIN_GUIDE.md#manage-local-users-in-admin) for the full workflow.

### Check a config file

```sh
olr config check -config /etc/olr/config.yaml   # or $OLR_CONFIG
docker compose exec -T router olr config check
```

It runs the router's strict parser (unknown keys, missing `${VAR}`/`bearer_token_env`
variables, invalid values) without starting, migrating or using the database or
network, and prints `{"valid":true,…}` or the problems with exit status 1.

### Add a model or a replica

Edit the config file — a new entry under `models` or a new `pool` replica with its `weight`
(relative throughput) and `max_inflight` — then restart the router. Restarts are graceful;
clients of streams still running after the grace period see an aborted stream. A replica is
used once it passes a successful health check; two failed checks mark it unhealthy.
Give access with `groups.<name>.models`, and add
aliases for the names clients send by default.

### Disable a user, revoke keys

```sh
olr admin user list -match alice
olr admin user disable -id <uuid>         # keys, sessions and forwarded calls stop at once
olr admin key list -user <uuid>
olr admin key revoke -id <key uuid>
```

The portal's **Admin** page can disable or re-enable users; operator key revocation
uses the CLI. Every router clears its API-key cache before the command returns.
Cached OIDC access-token principals are refused on their next request too.
A user disabled in Keycloak is disabled here by the group sync. Users can revoke their own keys.

### Change one user's limits

`olr admin override set -user <uuid> -field tokens_per_day -value 5000000 -reason "thesis"`
(`-value unlimited` lifts a limit), `override clear` and `override list`, or the **Admin** page.
Changes apply at the user's next request, for API keys and OIDC access tokens alike.

### Rotate secrets

| Secret | How |
|---|---|
| Postgres password | `ALTER ROLE olr PASSWORD '…'`, update `OLR_DATABASE_URL`, recreate the Compose container |
| `OIDC_PORTAL_CLIENT_SECRET`, `OIDC_SYNC_CLIENT_SECRET` | regenerate in Keycloak, update the environment, recreate the Compose container; logins in progress must start again, sessions stay. With managed settings, store the new value with `olr admin settings draft -secret`, activate and recreate |
| `OLR_METRICS_TOKEN` | set the new token in the router and in Prometheus's credentials file, recreate/restart the affected services so they read it |
| backend tokens (`bearer_token_env`) | configure the backend to accept old and new, update the router environment, recreate the Compose container, drop the old one |
| API keys | users create a new key and revoke the old one; keys expire after `auth.default_key_lifetime_days` |
| Local administrator password | `olr admin password change` with private current/new files; retire the bootstrap source after first change; if lost, `olr admin recover` |

### Back up Postgres

The database holds users, API-key/password hashes, sessions, overrides and usage.
API keys cannot be recovered. Upgrading for OLR-SEC-04 drops historical ciphertext;
retire legacy API-key encryption secrets and expire old backups per site policy.
Protect dumps as secrets and back it up daily:

```sh
pg_dump --format=custom --file=olr-$(date +%F).dump "$OLR_DATABASE_URL"
pg_restore --clean --if-exists --dbname="$OLR_DATABASE_URL" olr-YYYY-MM-DD.dump   # restore, router stopped
```

Keep backups no longer than the usage retention, or erasures (below) come back with a restore;
after restoring an old backup, repeat the erasures done since.

Managed settings (revisions, activations, encrypted secrets and the audit history)
are in the same database. A restore returns them to the backup's active revision;
stored secrets open only with the matching `OLR_SETTINGS_ENCRYPTION_KEY`. Without
it, routers start with SSO off and local login available.

A restore also returns the protected administrator's password, sessions and keys to
their state at backup time. If that password is unknown, run `olr admin recover`
after the restore.

Migration 0004 adds the credential table and protected-account constraints;
0005 adds credential revisions, scoped local sessions and login throttling. Back
up before upgrading. Downgrading to a binary that predates these invariants is
unsupported; restore a matching pre-upgrade backup if rollback is necessary.

### Connect Prometheus and a tracing backend

Set `OLR_METRICS_TOKEN` and scrape `/metrics` with it; set `OTEL_EXPORTER_OTLP_ENDPOINT` to a
collector or Jaeger (OTLP/HTTP). Scrape configuration, metric names, the Grafana dashboard and
span attributes are in [OBSERVABILITY.md](OBSERVABILITY.md); `make observability-up` shows the
whole setup locally.

### Log retention

The router logs one JSON line per request to stdout: request ID, user ID, 12-character key
prefix, model, replica, status, tokens and timing, never content. Keep these logs no longer
than `retention.usage_days`, e.g. `docker` log rotation with `max-size`/`max-file`, or the
retention of the log system they are shipped to.

## Limits and their errors

Calls pass, in order: authentication, model allow-list, concurrency (`max_concurrent` or
`max_concurrent_batch` for the key's class), `rpm`, `tpm`, `tokens_per_day`, `tokens_per_month`.
A refused call gets **429** with `Retry-After` (seconds) and the protocol's error shape:

```json
{"error":{"code":429,"type":"rate_limit_error","param":null,"limit":"tpm",
           "message":"tpm exceeded; retry at 2026-11-11T12:00:21Z","reset_at":"2026-11-11T12:00:21.4Z"}}
```

Anthropic clients get the same inside `{"type":"error","error":{…}}`. `limit` names the gate;
`reset_at` is when it opens again (for concurrency an estimate of one second). Other answers:
401 bad or missing credentials; 403 model not allowed, or unknown/disabled forwarded user;
400 malformed body or, with `strict_quota`, a call without `max_tokens`; 408 a body not sent
within two minutes; 413 a body over the configured limit (4 MiB by default); 503 the body-reader
budget is full, no replica is free after `queue_timeout_s`, or the database is unreachable.
Users see their own limits and use in the portal and at `/v1/me/usage`.

## Known limitations

- One router instance: concurrency, RPM and TPM counters, the queue and session affinity are in
  memory. More than one replica needs shared counters (Redis or Postgres), see PLAN §4.
- Endpoint/model parameters for existing logical models can be activated live through
  Admin. YAML/environment, logical model names, aliases and group policies require
  recreation. Endpoint activation does not synchronize multiple router processes.
- Quotas count tokens as the backend reports them; a stream that ends without usage is counted
  by its content events, a lower bound. Without `strict_quota`, calls already admitted can
  overshoot a quota.
- A call refused by `tpm` or a quota still counts against `rpm`.
- Per-request records are one table; monthly partitions are not used (see Usage retention).
- Exported traces and shipped logs are outside the router's erasure and retention.
- Users are re-enabled by an operator only, never by the group sync.


## Erase a user on request

Find the user, then delete them. Deletion removes the account with its keys, sessions,
per-request records (including calls a trusted forwarder made for the user) and daily totals
in one transaction, clears the API-key caches of every router before returning, and cannot
be undone:

```sh
docker compose exec -T router olr admin user list -match alice
docker compose exec -T router olr admin user delete -id <uuid>
```

A user who signs in again afterwards gets a new, empty account. To block access instead, use
`olr admin user disable -id <uuid>`. Request log lines and exported spans are outside the
database; see [PRIVACY.md](PRIVACY.md).

## Queueing, session affinity and priority

Per model:

- `queue_timeout_s`: when every replica is at `max_inflight`, a call waits up to this long for
  a free slot instead of getting 503 at once. Waiting calls are served in priority order, first
  come first served within a priority; a call that does not get a slot in time gets
  503 with `Retry-After: 1`. `olr_queue_waiting{model}` shows the queue. Keep it well below the
  reverse proxy's read timeout.
- `session_affinity: true`: calls with the same `X-Session-Id` header go to the same replica
  (weighted rendezvous hashing) while it is healthy and below `max_inflight`, so the backend's
  prefix cache is reused; otherwise they go to the least-loaded replica.
- `priority: {interactive: 0, batch: 10}`: the router sets the request field `priority` to the
  key class's value plus the caller's group `priority` (the lowest of their groups), replacing
  any value the client sent. Start vLLM with `--scheduling-policy priority`; lower values are
  scheduled first. Verified on vLLM 0.33.0 for `/v1/chat/completions` and `/v1/messages`. Without
  this setting the field passes through unchanged.

The router's own queue orders calls the same way — interactive before batch — with or without
`priority`; batch keys rank 10 after interactive ones by default.

## Federated login and the required claim

Federated login (an institutional IdP, eduGAIN or a community AAI) is Keycloak configuration:
add the upstream as an identity provider of the realm and map its attributes. The router keeps
trusting only Keycloak. The dev stack simulates this with a second realm, `upstream-idp`
(users `fed-member` and `fed-guest`, passwords `<user>-dev-only`), brokered into `olr-dev`:

- an attribute importer stores the upstream `affiliation` claim on the `olr-dev` user;
- an *advanced claim to group* mapper puts `affiliation = member@example.org` into `students`;
- the `olr-portal` client emits `affiliation` in the ID token.

As defence in depth the router refuses portal logins whose ID token lacks a claim:
`OIDC_REQUIRED_CLAIM=affiliation=member@example.org` (or `oidc.required_claim: {name, value}`).
A string claim must equal the value; an array must contain it. A refused login gets 403 and
creates no router user. Compose sets this default; the local dev users carry the affiliation.
`make brokered-check` signs in `fed-member` through the broker, creates and uses a key, and
checks that `fed-guest` is refused.

## Service tokens and group sync (Keycloak)

**Access tokens on `/v1/*`.** With `oidc.audience` (`OIDC_AUDIENCE`, e.g. `olr-api`) set, a
bearer that is not an API key is checked as an access token of the issuer: signature against
the JWKS (refetched when a token names an unknown key), `iss`, `aud` containing the audience,
`exp`. The token's subject becomes a router user (issuer + `sub`) with the token's groups, so
quotas and usage apply as for any user. A service obtains tokens with client credentials:

```sh
curl -s "$OIDC_ISSUER/protocol/openid-connect/token" -d grant_type=client_credentials \
  -d client_id=olr-service -d client_secret=... | jq -r .access_token
```

In Keycloak create a client `olr-api` (no flows; it is only the audience) and give each service
client an *audience* mapper that adds `olr-api` to access tokens, plus the groups mapper. The
dev realm has `olr-service` as an example. A user disabled in the router is refused within a
minute (verified tokens are cached for at most a minute).

**Group sync.** A confidential client `olr-sync` with *service accounts* enabled, the
`realm-management` role `view-users`, and on Keycloak 26.8 or later the `kc-admin-api`
client scope (its tokens must carry the admin-API audience). Set its secret in
`OIDC_SYNC_CLIENT_SECRET`. At startup and every `oidc.sync_interval_s` (default 900) one
router checks every enabled user of the issuer: removed or disabled in Keycloak → disabled here
with their keys and sessions; changed groups → stored, effective for existing keys at once.
Users are not re-enabled automatically: use `olr admin user list` and the portal's admin page,
or `PUT /portal/api/admin/users/{id}/enabled`. Each run logs `msg="group sync"` with counts, or
`group sync failed`.

## Open WebUI and other trusted forwarders

An application that serves many users with one key can attribute each call to its user:

1. Create a service user and a forwarder key:
   `olr admin user add -username open-webui -groups staff`, then
   `olr admin key create -user <uuid> -label open-webui -trusted-forwarder`.
2. Map the header the application sends, in the config file:
   `forwarders: {header_map: {X-OpenWebUI-User-Email: email}, unknown_user: reject}`.
   `X-OLR-End-User: <username>` always works.
3. In Open WebUI set `ENABLE_FORWARD_USER_INFO_HEADERS=true` and add the router as an OpenAI
   connection (`<router>/v1`) with the forwarder key.

Each call with the header is authorized against the end user's groups, counts against their
limits and quotas, and is recorded with `usage.forwarded_for` set to them, so it appears in
their portal and they can delete it. Users must exist in the router — they have signed in to
the portal once, or were added with `olr admin user add` — and their email must be unique.
Otherwise the call is refused with 403 (`unknown_user: reject`) or served as the forwarder's
own (`forwarder_policy`). A disabled user is always refused. The mapped headers are removed
before the call reaches a backend, and ignored from keys without the flag.

## Usage retention

The router deletes per-request records older than `retention.usage_days` (default 90) and
daily totals older than `retention.usage_daily_days` (default 400, at least 62 so that quotas
keep the current month) once at startup and every night at `retention.run_at` in
`limits.timezone`. Deletion runs in batches of 10,000 rows; with several routers, one deletes
while the others skip. Each run logs `msg=retention` with the number of deleted rows, or
`retention job failed`. To run it by hand:

```sh
docker compose exec -T router olr admin retention run   # prints {"usage":N,"usage_daily":M}
```

`usage` is one table indexed by time. Monthly partitions are not needed at pilot volume
(roughly 3 GB with indexes per 10 million requests); they can be added by a later migration if the table
grows too large to prune by index.

## Database migrations

Migrations in `internal/db/migrations` run at startup inside one transaction under an advisory
lock, so concurrent routers are safe and a failed migration changes nothing. `olr migrate`
applies them and prints the schema version, for a job that runs before the routers start.
Take a backup before upgrading; a router refuses a database with a newer schema, so roll back
by restoring the backup together with the previous image.

## Streaming load test

`cmd/olr-loadtest` compares identical synthetic streaming workloads directly against
the backends and through the router, separately for OpenAI and Anthropic. It also
checks cancellation, accounting and idle runtime state.

### Reproduce with Compose

Docker Compose is the only host dependency:

```sh
make loadtest > loadtest.json
```

The script creates a unique `olr-loadtest-<uid>-<pid>` project with a private network,
a disposable Postgres database, two mocks, a router and the load generator. It
creates a dedicated user and key, then removes that project and its data on exit.
There are no published ports. An existing development stack remains running.
Public test credentials apply only to this isolated stack. Secrets are passed in
environment variables and are absent from reports and command-line arguments.

The benchmark config is [loadtest.config.yaml](../deploy/compose/loadtest.config.yaml),
separate from the pilot's much smaller limits. Replica weights are 2:1, with 5 ms
and 10 ms token delays. Each replica has 8,192 slots; the user has 16,384 concurrency
slots and no RPM or token quota. These capacities let the experiment reach the
router's runtime/accounting limit instead of the pilot's admission limits.

The router and generator each have a four-CPU quota and 1 GiB memory limit; each
mock and Postgres have two CPUs and 512 MiB. The script reduces CPU quotas on hosts
with fewer CPUs. `OLR_LOADTEST_CPUS` and `OLR_LOADTEST_AUX_CPUS` can select a smaller
budget; record the values with any published result. Run on an otherwise quiet
machine. Shared CI or Docker Desktop scheduling can affect timing.

The JSON report includes per-protocol/per-run measurements, error categories,
intentional disconnect counts, replica distribution, CPU cores used, peak
RSS/heap/goroutines, and idle snapshots. A nonzero exit status means a check failed
or the experiment could not complete. Successful HTTP responses alone do not prove
that accounting or cancellation worked.

### Measurement and checks

- Both paths are warmed at full concurrency before measurement. A synchronized
  worker pool maintains up to `-concurrency` active clients until `-requests` finish.
  The direct path uses the configured weights and least-outstanding selection;
  direct and router order alternates across runs.
- Time to first token stops at the first nonempty content delta, excluding role
  and message-start events. `AddedP95TTFTMS` is `p95(router) - p95(direct)`, the
  difference of two distributions, not a paired per-request measurement. Its
  default budget is below 5 ms.
- Per-stream tokens/s is `(output_tokens - 1) / (last_content - first_content)`;
  aggregate tokens/s is completed output tokens divided by batch wall time.
  Both the median per-stream ratio and aggregate router/direct ratio must be at
  least 0.98. A terminal event and token usage are required; truncated responses,
  SSE error events, non-SSE responses and HTTP failures are errors.
- A separate batch disconnects 10% of clients after four content deltas. These
  calls are excluded from performance statistics. Mock cancellation counters
  must increase by at least the number of intentional disconnects.
- Replica gauges, mock in-flight counters and `/v1/me/usage.in_flight` must return
  to zero after each run. The tool waits for asynchronous persistence and verifies
  one `usage` row per router request for the dedicated key, including warm-ups and
  disconnects. No writer failures or drops are allowed.
- Client keep-alives are closed before idle snapshots. The initial warmed state
  is recorded separately from the post-run snapshots, because the upstream idle
  connection pool can still grow during the first measured run. Across three
  post-run quiet windows, the spread of `IdleGoroutinesMin` is at most four
  (health checks and scrapes). Metrics requests are serialized, and each window
  uses its minimum goroutine count after all requests and accounting have drained
  to exclude transient observation work. Persistent goroutines remain counted;
  `Goroutines` also retains the final point sample.
  Across three runs, heap spread must be no more than the larger of 32 MiB or 25%; RSS
  spread no more than 64 MiB or 25%. Raw values remain visible: finite-run bounds
  detect regressions, not every possible leak.

Defaults are 256 clients, 128 output tokens, twice as many requests as clients,
three runs and two seconds of quiet time before each idle snapshot. For longer
generations and one simultaneous wave per batch:

```sh
make loadtest LOADTEST_ARGS='-requests 256 -tokens 256' > loadtest-long.json
```

CI runs the same 256-client functional, accounting and leak checks with 32 tokens
and `-check-performance=false`. It preserves the JSON artifact, including measured
performance. Shared-runner timing is recorded without making it a pass/fail gate;
all functional checks remain enforced.

### Find the capacity boundary

Raise concurrency and remove the mock's generation delay, keeping resource quotas
fixed. Disconnections are tested in the paced run; disable them for this maximum
token-rate experiment because a mock may finish before a client can disconnect.

```sh
for n in 64 128 256 512 1024; do
  make loadtest LOADTEST_ARGS="-concurrency $n -requests $((n * 4)) -tokens 128 -mock-delay 0s -abort-fraction 0 -runs 1 -settle 1s -check-performance=false" \
    > "loadtest-fast-$n.json" || true
done
```

Each iteration is isolated and retains failures. Compare direct and router
throughput, router CPU, errors, usage drops and persisted rows. A generation-rate
cap in the mocks or a configured pool/user limit is not router saturation.
A router CPU plateau with faster direct throughput, or a full accounting queue
with missing rows, identifies a router-side boundary. Keep the load generator and
mocks out of CPU throttling before assigning a CPU limit to the router. Results
are specific to the recorded resource budget and workload.

### Existing backends

`make build` also builds `bin/olr-loadtest`. Set `OLR_API_KEY`, a separate
`OLR_METRICS_TOKEN`, and optionally `OLR_DIRECT_API_KEY` in the environment. Set
`OLR_LOADTEST_DATABASE_URL` to verify usage with a read-only role that can read
`api_keys` and `usage`. Omit `-mock` so no mock control headers are sent:

```sh
bin/olr-loadtest -router https://router.example.org \
  -direct https://replica-a.example.org,https://replica-b.example.org \
  -weights 2,1 -model chat-model -direct-model backend-model \
  -concurrency 256 -tokens 512 > loadtest-real.json
```

Use a dedicated key and sufficient limits. The tool does not modify configuration
or erase existing records. Without database credentials, `AccountingVerified`
stays false and the report states that limitation. Fewer than three repetitions
leave `LeakStabilityVerified` false and skip stability assertions. Real GPU/backend
and client validation remains tracked in
[#8](https://github.com/arm2arm/open-llm-router/issues/8).

### Recorded results

Measurements for [#18](https://github.com/arm2arm/open-llm-router/issues/18), including
the capacity boundary, are recorded below. The development stack on port 8080 is
not part of these experiments.

Recorded on 11 November 2026 with Go 1.27.4, Linux/arm64 containers on Docker
Desktop (18 vCPUs and 64 GiB available to the VM). Resource quotas are the Compose
defaults above: router/generator four CPUs each, mocks/Postgres two CPUs each.
These are synthetic local measurements, not a real-GPU capacity claim.

Strict acceptance command: `make loadtest > loadtest.json`. All six measured
batches reached 256 overlapping clients and completed without errors. All checks
passed, including both performance ratios on every repetition.

| Protocol | Run | Added p95 TTFT (ms) | Median tokens/s ratio | Aggregate tokens/s ratio |
|---|---:|---:|---:|---:|
| openai | 1 | 3.990 | 0.9988 | 1.0171 |
| anthropic | 1 | -2.051 | 1.0095 | 1.0091 |
| openai | 2 | 0.283 | 0.9943 | 1.0215 |
| anthropic | 2 | -0.653 | 1.0080 | 1.0095 |
| openai | 3 | 1.993 | 0.9801 | 1.0024 |
| anthropic | 3 | -0.835 | 0.9894 | 1.0282 |

The worst added p95 TTFT was 3.990 ms; the worst median rate ratio was 0.9801
(1.99% loss). Aggregate throughput was at least 1.0024 times direct throughput.
The router persisted all 6,656 requests, including 150 intentional disconnects,
with zero writer failures or drops. Every post-run replica/user/mock in-flight
counter was zero. Replica A handled 4,090 requests and B 2,054 after the baseline
(66.6% / 33.4%, matching the configured 2:1 weights).

Idle goroutines were 484, 486 and 486 after the three runs (533 at the initial
warmed snapshot). Idle RSS was 35.2, 55.3 and 59.0 MiB; heap in use was 10.5, 13.3
and 34.8 MiB. Both remain within the stated finite-run bounds. Peak RSS was 59.5
MiB and sampled peak router CPU was 1.04 cores. The full measurements are in
[the acceptance report](benchmarks/2026-11-11-arm64-256.json).

A regression found during this work was the expired-key lookup burst: 256
simultaneous requests could issue 256 identical database lookups. Refreshes now
coalesce to one lookup while the invalidation listener is healthy. Regression
tests also cover owner/waiter cancellation, revocation during refresh, and the
existing fail-closed behavior when the listener is lost.

The zero-delay experiment used four requests per client, 128 tokens, one run,
no intentional disconnections and performance gates disabled. This profile
finds the accounting boundary; it does not establish multi-run leak stability.

| Clients | Router requests | Persisted rows | Dropped rows | HTTP/stream errors | Sampled peak CPU (cores) |
|---:|---:|---:|---:|---:|---:|
| 256 | 3,072 | 3,072 | 0 | 0 | 2.48 |
| 512 | 6,144 | 5,454 | 690 | 0 | 3.30 |

**Observed boundary: the 1,024-record accounting queue starts dropping records
between 256 and 512 simultaneous clients in this unpaced burst profile.** At 512
clients the inference path still completed every call, but 690 of 6,144 records
were dropped. This is a router accounting bottleneck, not a mock generation-rate
limit: the queue-drop counter increased while all inference streams completed.
A prior exploratory 1,024-client run also dropped records. This is not a universal
maximum client count: token rate, burst shape, Postgres latency, pool limits and
resource quotas change the boundary. Do not size a deployment from successful
HTTP responses alone. Alert on `olr_usage_dropped_total` and
`olr_usage_write_failures_total`; reduce admission or improve persistence capacity
before running an equivalent sustained workload.

Raw capacity reports: [256 clients](benchmarks/2026-11-11-arm64-fast-256.json),
[512 clients](benchmarks/2026-11-11-arm64-fast-512.json). The 512-client command
correctly exits nonzero for dropped accounting; it is not an acceptance pass.
No reachable real vLLM/GPU backend was configured for this session; repeat on the
deployment hardware before the pilot.

## API-key policy rollout and lifecycle

Configure the same auth policy on all router replicas and CLI config files; restart replicas
after changing it. CLI callers without a config use the secure defaults (90 days, 10 keys).
Operators with database access are trusted and can change policy; there is no portal/admin
bypass or per-user exception. A site exception requires an operator-reviewed config change.
Existing keys are not silently shortened or revoked: inventory them before rollout, issue
replacements, then revoke over-age/indefinite keys. Accounts already above the cap cannot
issue again until revocation or expiry frees capacity. Disabled users cannot issue keys.

`olr admin key list -user UUID` reports created, last_used, expires, revoked and expired
without secrets. Schedule a daily read-only inventory job using the query below, send
expiry reminders at seven days to the account owner through your approved notification
system, and open a rotation ticket for keys unused for 30 days. Never email raw keys.
Authentication rejects expired keys automatically; a scheduled operator job should call
`olr admin key revoke -id UUID` for approved inactive/legacy revocations so cache invalidation
reaches all replicas. Use the same command immediately during incident response.

```sql
SELECT user_id, id, label, created, last_used, expires,
       clock_timestamp()-created AS age,
       expires <= clock_timestamp()+interval '7 days' AS expires_soon
FROM api_keys
WHERE NOT revoked AND
 (expires IS NULL OR expires <= clock_timestamp()+interval '7 days'
  OR coalesce(last_used,created) < clock_timestamp()-interval '30 days');
```
