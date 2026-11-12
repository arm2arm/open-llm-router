# open-llm-router Helm chart

One release installs the router/portal, PostgreSQL 17, Keycloak demo SSO and an
OpenAI-compatible mock backend. PostgreSQL and Keycloak use persistent volumes by
default. Initial credentials are generated automatically or supplied in one values
file. The router waits for the bundled database and Keycloak before starting.

Follow [the installation guide](../../docs/INSTALL.md#kubernetes-with-helm) for
image building, first login, streaming and upgrades. Build the router and mock
images from this checkout; no public application image or chart registry is
assumed. For kind, load `olr-router:local` and `olr-mock-llm:local`; for another
cluster, push both to your registry and edit their repositories/tags.

```sh
cp charts/open-llm-router/examples/demo.values.yaml /private/olr/values.yaml
# Edit images and any initial credentials/endpoints in the copy.
helm upgrade --install olr charts/open-llm-router \
  --namespace olr --create-namespace \
  -f /private/olr/values.yaml --wait --timeout 10m
```

Use Helm 4 and a cluster compatible with your Helm version. Kubernetes 1.27+ is the
chart's API floor. The default needs a default StorageClass, or explicit storage
classes in the values file. The chart has no dependency downloads or separate
infrastructure manifests. Helm's install notes show port-forward and password
retrieval commands. With the example release name, forward the router Service
`olr-open-llm-router` on `8080:80` and Keycloak Service
`olr-open-llm-router-keycloak` on `8081:8080` in separate terminals.

- Local protected administrator: `http://localhost:8080/portal/login/local`, user
  `admin`; password key `OLR_BOOTSTRAP_ADMIN_PASSWORD`. Change it on first login.
- Portal SSO: `http://localhost:8080/portal/`, user `staff` in group `staff`;
  password key `OLR_DEMO_USER_PASSWORD`.
- Keycloak console: `http://localhost:8081/admin/`, user `admin`; password key
  `KC_BOOTSTRAP_ADMIN_PASSWORD`.

The default Secret is `olr-open-llm-router-credentials`:

```sh
kubectl -n olr get secret olr-open-llm-router-credentials \
  -o jsonpath='{.data.OLR_BOOTSTRAP_ADMIN_PASSWORD}' | base64 --decode; echo
```

The bundled backend returns deterministic demo text for `chat-model`; no GPU
models are downloaded. Bundled Keycloak uses `start-dev` with a persistent H2
store for evaluation. Use external production SSO for a public installation.

## Values

See [values.yaml](values.yaml) for all defaults,
[examples/demo.values.yaml](examples/demo.values.yaml) for initial credentials and
endpoints, and [examples/production.yaml](examples/production.yaml) for external
services and HTTPS ingress.

| Value | Default | Purpose |
|---|---|---|
| `image.repository`, `image.tag`, `image.digest` | `olr-router`, `local`, empty | Router image; digest overrides tag |
| `image.pullPolicy`, `imagePullSecrets` | `IfNotPresent`, `[]` | Image policy and pull Secrets; pull Secrets apply to all workloads |
| `existingSecret` | Empty | Empty creates credentials; otherwise reference an existing same-namespace Secret |
| `secrets.bootstrapAdminPassword` | Generated | Initial local `admin` password, at least 15 characters |
| `secrets.env` | `{}` | Additional secret environment values, such as backend API tokens |
| `postgresql.enabled`, `image` | `true`, `postgres:17` | Bundled single-instance PostgreSQL |
| `postgresql.username`, `database`, `password` | `olr`, `olr`, generated | Database initialization; keep stable after first install |
| `postgresql.persistence.enabled`, `size`, `storageClass` | `true`, `8Gi`, default class | Database PVC; `false` uses ephemeral storage |
| `externalDatabase.url` | Empty | Required with bundled PostgreSQL disabled and no `existingSecret` |
| `keycloak.enabled`, `image` | `true`, `quay.io/keycloak/keycloak:26.8.2` | Bundled development SSO and demo realm |
| `keycloak.baseURL` | `http://localhost:8081` | Browser-visible Keycloak origin; internal backchannel is automatic |
| `keycloak.adminPassword`, `clientSecret`, `demoUserPassword` | Generated | Initial console, portal-client and staff credentials |
| `keycloak.persistence.enabled`, `size`, `storageClass` | `true`, `1Gi`, default class | Demo Keycloak data PVC |
| `demoBackend.enabled` | `true` | Deploy the mock backend for `chat-model` |
| `demoBackend.image.repository`, `tag`, `pullPolicy` | `olr-mock-llm`, `local`, `IfNotPresent` | Demo image built from the Dockerfile's `mock-llm` target |
| `portal.baseURL`, `cookieSecure` | `http://localhost:8080`, `false` | Browser origin; HTTPS and `true` for public access |
| `config` | Demo model and staff/admin policies | Router YAML; `${VAR}` references expand in the router |
| `env` | `{}` | Non-secret environment variables; disable bundled Keycloak for external `OIDC_*` |
| `replicaCount` | `1` | Enforced single router process, with `Recreate` rollouts |
| `service.type`, `port`, `annotations` | `ClusterIP`, `80`, `{}` | Router Service exposure |
| `ingress.enabled`, `className`, `host`, `tlsSecretName`, `annotations` | Disabled | Existing controller and TLS Secret |
| `resources` | 500m/256Mi request, 1Gi memory limit | Router resources |
| `postgresql.resources`, `keycloak.resources`, `demoBackend.resources` | See values | Per-component resources |
| `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` | Empty | Router annotations and placement |
| `nameOverride`, `fullnameOverride` | Empty | Keep resource names stable across upgrades |

`OLR_CONFIG`, `PORTAL_BASE_URL`, `PORTAL_COOKIE_SECURE`, `OLR_DATABASE_URL` and
`OLR_DEMO_BACKEND_URL` are reserved in `env`. Bundled Keycloak manages `OIDC_*`;
set `keycloak.enabled: false` to configure external OIDC. Quote environment values,
even booleans/numbers. Helm merges maps but replaces lists: provide the complete
`pool` list when changing a backend. Remove an inherited model/group with `null`.
The router validates the full configuration at startup.

For a real model, set `demoBackend.enabled: false`, replace
`config.models.chat-model.upstream_model` and its full `pool` list. Tokens belong
in `secrets.env` or `existingSecret`; refer to them with `bearer_token_env` in the
pool. `env.OLR_BACKEND_ALLOWED_CIDRS` and `env.OLR_BACKEND_TOKEN_ENVS` control extra
destinations/token names selectable in Admin. Saved Admin endpoint revisions
continue to override YAML until restored to file configuration.

## External services and Secrets

The production example explicitly disables `postgresql`, `keycloak` and
`demoBackend`, preserves a real model pool and references `existingSecret`.
This is also the migration path from chart 0.1.x, which used external services.
Enabling a bundled database does not migrate an existing database.

When `existingSecret` is set, the chart does not generate or modify that Secret.
The referenced Secret must provide:

| Secret key | Required for |
|---|---|
| `OLR_DATABASE_URL` | Every install; for bundled PostgreSQL, point it at `<release>-open-llm-router-postgresql:5432` with matching credentials |
| `OLR_BOOTSTRAP_ADMIN_PASSWORD` | First local admin creation; omitted after initialization if desired |
| `OLR_SETTINGS_ENCRYPTION_KEY` | Optional: secrets stored with `olr admin settings`; a separate stable base64-encoded 32-byte key. The chart does not generate it |
| `POSTGRES_PASSWORD` | Bundled PostgreSQL; match the password in `OLR_DATABASE_URL` |
| `KC_BOOTSTRAP_ADMIN_PASSWORD` | Bundled Keycloak console administrator |
| `OLR_DEMO_USER_PASSWORD` | Bundled Keycloak's `staff` user |
| `OIDC_PORTAL_CLIENT_SECRET` | Bundled Keycloak, or confidential external portal client |

Add other OIDC, backend, metrics or OTLP secrets as needed. To use an external
database while generating the other credentials in Helm, leave `existingSecret`
empty, set `postgresql.enabled: false` and supply `externalDatabase.url`.

### Public ingress

Public ingress also requires the controller settings in
[`deploy/nginx/f5-controller-config.yaml`](../../deploy/nginx/f5-controller-config.yaml),
merged into the F5 controller's existing ConfigMap. The production annotations set a
4 MiB body cap and per-client request rate; the controller config sets a 120 s body
inactivity timeout, disables request buffering and limits concurrent requests per client.
See the [runbook](../../docs/RUNBOOK.md#behind-a-tls-reverse-proxy) for other controllers,
client IP handling and memory sizing. Match `config.limits.max_body_bytes` and the edge
body cap. Upload admission defaults to 32 globally and 4 per user, further capped by the
user's effective inference concurrency; these settings remain bounded for unlimited groups.

## Credentials and data lifecycle

Blank credentials are generated on first install; Helm `lookup` reuses their
installed Secret values on upgrade and reinstall. Set explicit credentials before
initialization. Helm stores both supplied values and rendered Secrets in release
history, so protect access to that history as well as the Secret itself.
`helm template` is an offline preview and generates fresh random placeholders;
use cluster-connected install/upgrade to retain credentials. Do not apply an
offline render over an existing release.

PostgreSQL initialization settings and Keycloak realm import apply once. Updating
values does not rotate database passwords or edit an existing realm. The chart
rejects a changed bundled database password; rotate it in PostgreSQL and the
Secret together. Change Keycloak users/client secrets and portal redirect/logout
origins in Keycloak's console, then synchronize the deployment values/Secret.
The local bootstrap password is ignored after the administrator is initialized.
API keys are hash-only and shown once. Recovery settings were removed; upgrades
drop historical ciphertext. Remove legacy API-key encryption secrets and replace lost keys.

Changes to `config` update the router's checksum; image/environment changes also
roll out its pod. Changes to Secret values require restarting the consumers.
Rollback does not restore database data/migrations, Keycloak state or Admin-saved
endpoints. Back up before upgrading; see the [runbook](../../docs/RUNBOOK.md).

`helm uninstall` removes workloads and Services, but retains the chart-created
credentials Secret and enabled PVCs with `helm.sh/resource-policy: keep`. Reinstall
the same release name/namespace and values to reuse them. Disabling persistence
uses `emptyDir` and loses that component's data when its pod is replaced.

All workloads run without service-account tokens, as non-root users, with
RuntimeDefault seccomp, dropped capabilities and no privilege escalation. The
router and mock backend have read-only root filesystems; Keycloak needs a writable
installation for its development startup. Probes distinguish liveness and
readiness. The router gets 35 seconds to drain streams during shutdown.

## Validate the chart

`tests/helm/check_chart.py` uses Helm and PyYAML 6.0.3 to check the complete stack,
schema, isolated Service selectors, secret/database wiring, storage, external
services, security and packaging.

`bash tests/helm/acceptance.sh` needs Helm 4, kind, Docker, kubectl and Python 3.
It creates its own cluster/kubeconfig, builds the two images and installs all
components with one Helm command. It verifies local admin setup, real SSO login
and logout, streaming, stable generated credentials through upgrade/rollback,
and data/key reuse after uninstall/reinstall. Its trap removes its cluster;
it never selects the caller's cluster. Both checks run in CI. Override `HELM` and
`KIND` with binary paths when needed.

### Network isolation and production TLS

NetworkPolicies are enabled by default. Router ingress is limited to the configured
`networkPolicy.ingressPeers` and `monitoringPeers`; DNS and enabled demo dependencies
have explicit rules. Add external dependency peers and ports with `networkPolicy.egress`.
Use a policy-capable CNI and maintain dependency-side ingress restrictions. In production,
use the production example's `config.security.require_tls: true`, external TLS services,
and `sslmode=verify-full` for PostgreSQL. The chart refuses bundled demo services or
disabled policy in this mode. `tls.caSecretName` mounts CA files at `/etc/olr/ca`.
See the [network/TLS runbook](../../docs/RUNBOOK.md#kubernetes-network-and-transport-policy)
for admin-surface restrictions, private CAs, DNS and deployment acceptance probes.
