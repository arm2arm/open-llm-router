# open-llm-router Helm chart

Installs the router Deployment, ConfigMap, Service and optional Ingress. PostgreSQL,
model servers and Keycloak are external. The chart uses a pre-existing Kubernetes
Secret; it does not put credentials into values or generate new passwords on upgrades.

Follow [the installation guide](../../docs/INSTALL.md#kubernetes-with-helm) for image
building, database/Secret preparation, first login, streaming verification and upgrades.
The chart is installed from this checkout; no public image, Helm repository or OCI
chart registry is published by this change.

```sh
helm lint charts/open-llm-router
helm upgrade --install olr charts/open-llm-router \
  --namespace olr --create-namespace \
  -f /private/olr/values.yaml --wait --timeout 5m
```

Use Helm 4 and a Kubernetes cluster compatible with your Helm version. The chart uses
stable Deployment, Service and Ingress APIs and declares Kubernetes 1.27+; this is an
API floor, not a recommendation to run an unsupported Kubernetes release.

| Value | Default | Purpose |
|---|---|---|
| `image.repository`, `image.tag` | `olr-router`, `local` | Image built from the checkout; set your registry and tested tag |
| `image.digest` | empty | Optional `sha256:…`; overrides tag |
| `image.pullPolicy`, `imagePullSecrets` | `IfNotPresent`, `[]` | Registry access |
| `existingSecret` | `olr-secrets` | Same-namespace Secret, including `OLR_DATABASE_URL` |
| `portal.baseURL`, `portal.cookieSecure` | `http://localhost:8080`, `false` | Port-forward defaults; use HTTPS/`true` for published access |
| `config` | Example Ollama pool and staff/admin policies | Router YAML; `${VAR}` references expand in the router, never as Helm templates |
| `env` | `{}` | Non-secret environment variables; secrets come from `existingSecret` |
| `replicaCount` | `1` | Enforced single process; strategy is `Recreate` |
| `service.type`, `service.port`, `service.annotations` | `ClusterIP`, `80`, `{}` | Service exposure |
| `ingress.enabled`, `className`, `host`, `tlsSecretName`, `annotations` | Disabled | Existing controller and TLS Secret |
| `resources` | 500m/256Mi request, 1Gi memory limit | Router resources, not GPU capacity |
| `podAnnotations`, `nodeSelector`, `tolerations`, `affinity` | Empty | Placement and pod annotations |
| `nameOverride`, `fullnameOverride` | Empty | Resource naming; keep stable across upgrades |

`OLR_CONFIG`, `PORTAL_BASE_URL` and `PORTAL_COOKIE_SECURE` are reserved; use `config`
and `portal` instead of overriding them in `env`. Quote all `env` values, even
booleans/numbers. Helm merges maps but replaces lists: provide the complete replica
`pool` when overriding it. To remove an inherited model/group, set its key to `null`.
The router validates its full YAML on startup.

The Secret may also hold `OLR_BOOTSTRAP_ADMIN_PASSWORD`, the stable
`OLR_API_KEY_ENCRYPTION_KEY`, `OIDC_PORTAL_CLIENT_SECRET`, `OIDC_SYNC_CLIENT_SECRET`,
`OLR_METRICS_TOKEN`, backend credentials and OTLP authentication headers. Configure
backend `bearer_token_env` references in `config`; Admin-selectable extra token names
go in `env.OLR_BACKEND_TOKEN_ENVS`. A raw bootstrap password has no default and must
be replaced on first login. Prefer your existing secret manager for provisioning.

Configuration changes alter a pod checksum; `env`, image and other pod changes also
trigger a `Recreate` rollout. Changing an external Secret alone requires
`kubectl rollout restart`; keep the encryption key unchanged across updates.
Live endpoint revisions saved by Admin override the YAML pool until restored to
file configuration. Helm rollback does not roll back those revisions or PostgreSQL
migrations/data. See [upgrade instructions](../../docs/INSTALL.md#upgrade-and-rollback).

For [F5 NGINX Ingress Controller](https://docs.nginx.com/nginx-ingress-controller/install/helm/open-source/),
the supplied [production example](examples/production.yaml) uses its
[`nginx.org` annotations](https://docs.nginx.com/nginx-ingress-controller/configuration/ingress-resources/advanced-configuration-with-annotations/)
to disable response buffering, allow 32 MiB bodies and set 960-second timeouts.
Translate those settings for another controller. The older Kustomize/acceptance
manifests use the separate community ingress-nginx controller, which
[retired in March 2026](https://kubernetes.io/blog/2026/03/30/kubernetes-v1-36-sneak-peek/).
Use a maintained controller for new deployments.

The router runs as UID/GID 65532 with no privilege escalation, a read-only root
filesystem, dropped capabilities, no service-account token and a writable `/tmp`.
Probes distinguish process liveness from database/model-pool readiness. Shutdown
allows 35 seconds for the router's 30-second stream-draining grace.

## Validate the chart

`tests/helm/check_chart.py` uses Helm and PyYAML 6.0.3 to check rendering, schema,
release isolation, security settings, configuration rollouts and packaging.
`bash tests/helm/acceptance.sh` needs Helm 4, kind, Docker, kubectl and Python 3.
It creates its own cluster and kubeconfig, builds test images and exercises install,
first local-admin password change, streamed inference, upgrade, rollback and
uninstall. Its trap removes the cluster; it never selects the caller's cluster.
Both checks run in CI. Override `HELM`/`KIND` with binary paths when needed.
