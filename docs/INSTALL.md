# Installation

Use **Docker Compose** for a complete local environment with PostgreSQL, Keycloak
and mock backends. Use **Helm** for the same complete demo on Kubernetes, with
PostgreSQL, Keycloak SSO and a mock model backend in one release. Neither method downloads or runs GPU models for you.

| Path | Includes | Start here |
|---|---|---|
| Local Compose | Router, persistent PostgreSQL, development Keycloak, two mock model servers | [Local Docker Compose](#local-docker-compose) |
| Kubernetes / Helm | Router, persistent PostgreSQL, development Keycloak SSO, mock backend, optional Ingress | [Kubernetes with Helm](#kubernetes-with-helm) |
| Server Compose with external services | Router with your production PostgreSQL, Keycloak and GPUs | [Production Compose](#production-compose) |

Run commands from the repository root unless a step says otherwise. The examples
use a POSIX shell (Linux, macOS, or WSL). Keep one router process: rate limits,
concurrency counters, queueing and affinity are not shared across instances.

## Local Docker Compose

### 1. Prepare the checkout and local settings

Install Git and Docker with the Compose plugin **2.20+**. Check the tools:

```sh
git --version
docker version
docker compose version
git clone https://github.com/arm2arm/open-llm-router.git
cd open-llm-router
```

Docker builds the Go and React application; you do not need Go, Node or a GPU on
the host. On Docker Desktop, use Linux containers.

```sh
umask 077
# Keep an existing .env; it may contain your installation's secrets/settings.
test -f .env || cp .env.example .env
chmod 600 .env
openssl rand -base64 32
```

Edit `.env` before the first start:

```dotenv
# Choose a unique temporary password, at least 15 characters.
OLR_BOOTSTRAP_ADMIN_PASSWORD=REPLACE_WITH_A_PRIVATE_TEMPORARY_PASSWORD
# Paste the generated base64 value; keep it stable and back it up separately.
OLR_API_KEY_ENCRYPTION_KEY=REPLACE_WITH_GENERATED_BASE64_VALUE
# Optional, a second generated value: only needed to store SSO client secrets
# in managed settings (olr admin settings). Keep it stable and backed up too.
# OLR_SETTINGS_ENCRYPTION_KEY=REPLACE_WITH_ANOTHER_GENERATED_BASE64_VALUE
```

Enable encryption before issuing keys if admins should be able to copy them
later. Older hash-only keys cannot be recovered. The bootstrap creates a local
router account named `admin`, independently of Keycloak, with no default password.

### 2. Start the complete stack

```sh
docker compose config --quiet
docker compose up --build -d --wait --wait-timeout 180
docker compose ps
curl --fail http://localhost:8080/healthz
curl --fail http://localhost:8080/readyz
```

Both HTTP checks should return 200; readiness reports `{"status":"ready"}`.
The first build and Keycloak startup can take several minutes. If the wait times
out, inspect `docker compose logs --tail=100 router keycloak postgres`.

| Service | Address | Login |
|---|---|---|
| Router / portal | `http://localhost:8080/portal/` | Local `admin` with your bootstrap password, or development SSO below |
| Keycloak administration | `http://localhost:8081/admin/` | `admin` / `keycloak-admin-dev-only` |
| Mock backends | `http://localhost:8001`, `http://localhost:8002` | No login; deterministic canned replies |
| PostgreSQL | `localhost:5432` | Development database/user `olr`, password `olr-dev-only` |

The stack binds published ports to loopback. Its public development passwords and
Keycloak `start-dev` setup are for local evaluation. If ports are occupied, change
the port variables in `.env`; update `PORTAL_BASE_URL` and `OIDC_ISSUER` to match.
Changing a PostgreSQL password variable does not change an initialized database.

### 3. Complete administrator setup and issue a user key

1. Open <http://localhost:8080/portal/login/local>. Sign in as `admin` using the
   bootstrap password. Enter it as **Current password**, choose a replacement,
   and sign in again. The sidebar should say **Protected local administrator**.
2. In **Admin → Create local user**, create a user and select group **staff**.
   Sign in as that user, complete its password change, then open **API keys →
   Create key** and save the returned key.
3. Alternatively, use **Continue with Keycloak** and sign in as
   `staff` / `staff-dev-only`. The development SSO administrator is
   `admin` / `admin-dev-only`; it is separate from the local protected account.
4. After the local administrator password change, remove
   `OLR_BOOTSTRAP_ADMIN_PASSWORD` from `.env` and recreate the router:

```sh
docker compose up -d --no-deps --force-recreate router
```

The initialized local account and its new password remain in PostgreSQL. The
bootstrap input does not reset an existing account. Local users and SSO users can
both log in; additional local users require no Keycloak setup.

If the local administrator password is lost, write a temporary one to a private
host file and redirect it into the container, then sign in and replace it:

```sh
docker compose exec -T router olr admin recover -password-stdin < /private/secrets/olr-recovery
```

See [recovery](ADMIN_GUIDE.md#recover-the-protected-local-administrator) for its
effects on sessions and API keys.

**Upgrading an existing Keycloak setup:** sign-out now returns to the public
homepage. Before upgrading, add your portal origin followed by `/` (for example,
`http://localhost:8080/`) to the client's **Valid post logout redirect URIs**.
The updated development realm import applies only to new realms; update existing
clients in Keycloak's Admin Console. Keep the old `/portal/` URI allowed while
older application instances are still running.

### 4. Verify model listing and streaming

Set the router key in your shell, then run:

```sh
export OLR_API_KEY='PASTE_THE_ROUTER_KEY_HERE'
curl --fail http://localhost:8080/v1/models \
  -H "Authorization: Bearer $OLR_API_KEY"
curl --fail -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $OLR_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Expect SSE `data:` events and a final `data: [DONE]`. `curl -N` displays chunks as
they arrive. Open **Usage** to see the request. The default stack proves routing,
login and accounting using mocks; switch to a real model for useful answers.

### 5. Connect Ollama or another real backend

List models on your Ollama server first:
replace the example `192.168.1.50` with your server's address.

```sh
curl --fail http://192.168.1.50:11434/api/tags
```

Use a model name returned by that server. Edit these entries in the root `.env`:

```dotenv
OLR_CONFIG_FILE=../../config.ollama.example.yaml
OLLAMA_BASE_URL=http://192.168.1.50:11434
OLLAMA_MODEL=REPLACE_WITH_EXACT_INSTALLED_MODEL_NAME
OLR_BACKEND_ALLOWED_CIDRS=192.168.1.0/24
```

Then recreate the router and repeat the health/model/streaming checks:

```sh
docker compose up -d --no-deps --force-recreate router
curl --fail http://localhost:8080/readyz
```

`OLR_CONFIG_FILE` is relative to `deploy/compose`, hence the `../../` prefix.
The container must reach the backend: `localhost` inside it means the container.
For Ollama on the Docker Desktop host, use `http://host.docker.internal:11434`.
On Linux, use a reachable host/LAN address or configure Docker's host-gateway
mapping. Ollama must listen on an address reachable from Docker; allow access
from the router host in your firewall. Do not expose an unauthenticated model
server directly to the public Internet.

In **Admin → Endpoint configuration → Edit**, use **Detect models** to choose the
upstream name and **Test connections**, then **Save and apply**. The health path
is `/` for Ollama and usually `/health` for vLLM/SGLang. Clients send the public
name `chat-model`; **Upstream model name** is the exact name served by the backend.
Enable only protocols the backend implements. Thinking, tools and image inputs
pass through when supported by that model; protocol selection does not translate
OpenAI calls into Anthropic calls.

Saved Admin endpoint revisions override the file configuration for that model,
including after a restart. If an existing installation keeps its old pool, use
the endpoint editor's restore-to-file action before expecting a changed YAML
pool to apply. New logical models, aliases, group defaults and Keycloak settings
still require YAML/environment configuration and a router recreation.

### 6. Stop or update the local installation

```sh
# Stop containers and keep PostgreSQL / Keycloak named volumes.
docker compose down
# After selecting the desired tested checkout:
git pull --ff-only
docker compose up --build -d --wait --wait-timeout 180
```

Back up PostgreSQL and the encryption key before upgrading an installation with
data you need. Do not add `--volumes` to `down` unless you intend to delete its
database and Keycloak state. See the [backup/restore runbook](RUNBOOK.md).

## Kubernetes with Helm

The default [chart](../charts/open-llm-router) installs the **complete demo stack
in one Helm release**: router and portal, persistent PostgreSQL 17, persistent
development Keycloak with a configured SSO client and `staff` user, and a mock
model backend serving `chat-model`. Passwords and the API-key encryption key are
generated automatically. No database setup, separate manifests or GPU is needed.

### 1. Prepare the cluster and images

You need Kubernetes, `kubectl`, [Helm 4](https://helm.sh/docs/intro/install/), and a
**default StorageClass** (or set `postgresql.persistence.storageClass` and
`keycloak.persistence.storageClass`). Check your context before installing:

```sh
kubectl config current-context
kubectl cluster-info
kubectl get storageclass
helm version
```

Build the router and demo backend from this checkout; no public application image
or published Helm repository is assumed. For a remote cluster, push both images
to a registry its nodes can pull from:

```sh
export OLR_IMAGE_REPOSITORY=registry.example.org/olr
export OLR_MOCK_IMAGE_REPOSITORY=registry.example.org/olr-mock
export OLR_IMAGE_TAG=$(git rev-parse --short=12 HEAD)
# Use linux/arm64 for ARM nodes.
export OLR_IMAGE_PLATFORM=linux/amd64
docker login registry.example.org
docker buildx build --platform "$OLR_IMAGE_PLATFORM" --target olr \
  -t "$OLR_IMAGE_REPOSITORY:$OLR_IMAGE_TAG" --push .
docker buildx build --platform "$OLR_IMAGE_PLATFORM" --target mock-llm \
  -t "$OLR_MOCK_IMAGE_REPOSITORY:$OLR_IMAGE_TAG" --push .
```

For an existing local **kind** cluster, use local images instead:

```sh
docker build --target olr -t olr-router:local .
docker build --target mock-llm -t olr-mock-llm:local .
kind load docker-image olr-router:local olr-mock-llm:local --name YOUR_CLUSTER
```

### 2. Copy one demo values file and install everything

```sh
umask 077
export OLR_SITE="$HOME/.config/olr-k8s"
mkdir -p "$OLR_SITE"
chmod 700 "$OLR_SITE"
# Keep an existing configuration when repeating these instructions.
test -f "$OLR_SITE/values.yaml" || \
  cp charts/open-llm-router/examples/demo.values.yaml "$OLR_SITE/values.yaml"
chmod 600 "$OLR_SITE/values.yaml"
```

For remote clusters, edit `image.repository` / `image.tag` and
`demoBackend.image.repository` / `demoBackend.image.tag` to the images you pushed.
For a private registry, create its pull Secret in namespace `olr` and set
`imagePullSecrets: [{name: YOUR_SECRET}]`; it applies to all bundled components.
The local kind image names already match the demo values.

The [demo values file](../charts/open-llm-router/examples/demo.values.yaml) shows
all initial settings in one place:

| Value | Initial behavior |
|---|---|
| `secrets.bootstrapAdminPassword` | Blank generates the local `admin` bootstrap password; a supplied password must be at least 15 characters |
| `secrets.apiKeyEncryptionKey` | Blank generates a base64-encoded 32-byte key; optionally supply `openssl rand -base64 32` output and retain it |
| `postgresql.username`, `database`, `password` | Creates database/user `olr`; blank password is generated |
| `keycloak.adminPassword` | Blank generates the Keycloak console `admin` password |
| `keycloak.clientSecret` | Blank generates and shares the portal SSO client secret |
| `keycloak.demoUserPassword` | Blank generates the SSO `staff` user's password |
| `portal.baseURL`, `keycloak.baseURL` | Browser addresses `http://localhost:8080` and `http://localhost:8081` |
| `config.models.chat-model` | Default endpoint is this release's bundled demo backend |
| `secrets.env` | Optional backend API keys, metrics tokens and other secret environment variables |

Leave credentials blank for automatic generation, or choose them **before the
first install**. Helm stores supplied values and rendered Secrets in release
history; protect both the private file and access to Helm release Secrets. For
credentials managed outside Helm, use `existingSecret` as described in the
[chart reference](../charts/open-llm-router/README.md).

```sh
helm upgrade --install olr charts/open-llm-router \
  --namespace olr --create-namespace -f "$OLR_SITE/values.yaml" \
  --wait --timeout 10m
kubectl -n olr get pods,svc,pvc
```

This single Helm command deploys and waits for all components. The router waits
for PostgreSQL and applies its database migrations automatically. The first
Keycloak startup can take several minutes. Generated credentials are reused on
subsequent installs/upgrades against the same namespace and release name.

### 3. Open the portal and try both login methods

Keep each port-forward running in its own terminal:

```sh
kubectl -n olr port-forward service/olr-open-llm-router 8080:80
```

```sh
kubectl -n olr port-forward service/olr-open-llm-router-keycloak 8081:8080
```

Read the initial passwords from the release's Secret:

```sh
# Local router administrator (username: admin)
kubectl -n olr get secret olr-open-llm-router-credentials \
  -o jsonpath='{.data.OLR_BOOTSTRAP_ADMIN_PASSWORD}' | base64 --decode; echo
# Demo SSO user (username: staff)
kubectl -n olr get secret olr-open-llm-router-credentials \
  -o jsonpath='{.data.OLR_DEMO_USER_PASSWORD}' | base64 --decode; echo
# Keycloak administration console (username: admin)
kubectl -n olr get secret olr-open-llm-router-credentials \
  -o jsonpath='{.data.KC_BOOTSTRAP_ADMIN_PASSWORD}' | base64 --decode; echo
```

Open [local admin login](http://localhost:8080/portal/login/local), sign in as
`admin`, complete the required password change, and sign in again. This account
is independent of Keycloak; the bootstrap input never resets an existing account.
To reset a lost password, run
`kubectl -n olr exec -i deployment/olr-open-llm-router -- olr admin recover -password-stdin < FILE`
([details](ADMIN_GUIDE.md#recover-the-protected-local-administrator)).
Open the [portal](http://localhost:8080/portal/) in a separate browser session,
choose **Continue with Keycloak**, and sign in as `staff`. The demo realm assigns
that user the `staff` policy. Create an API key in that user's portal.
The [Keycloak console](http://localhost:8081/admin/) manages SSO accounts separately.

```sh
export OLR_BASE_URL=http://localhost:8080
curl --fail "$OLR_BASE_URL/healthz"
curl --fail "$OLR_BASE_URL/readyz"
export OLR_API_KEY='PASTE_THE_ROUTER_KEY_HERE'
curl --fail "$OLR_BASE_URL/v1/models" \
  -H "Authorization: Bearer $OLR_API_KEY"
curl --fail -N "$OLR_BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $OLR_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Expect incremental events and `[DONE]`, with deterministic demo text. Usage should
appear in the portal. No real model is downloaded or run by this chart.

### 4. Configure real endpoints or production services

To use Ollama, vLLM or SGLang, set `demoBackend.enabled: false` and replace the
upstream model and **complete** `pool` list in your values file, then run the same
Helm command. For example:

```yaml
demoBackend:
  enabled: false
config:
  models:
    chat-model:
      upstream_model: exact-installed-model-name
      pool:
        - url: http://ollama.models.svc.cluster.local:11434
          health_path: /
          weight: 1
          max_inflight: 1
```

For an authenticated backend, add its token under `secrets.env.GPU_API_TOKEN` and
`bearer_token_env: GPU_API_TOKEN` to the pool entry. `localhost` inside a pod is
that pod; use a Service DNS name or another reachable server address. Use `/` for
Ollama health checks and the backend's actual health path for other servers.
Saved Admin endpoint revisions override YAML pools until restored to file
configuration in the endpoint editor.

For public production access, start with the
[production values example](../charts/open-llm-router/examples/production.yaml).
It disables the bundled demo backend, development Keycloak and PostgreSQL, and
uses external services with a pre-existing `olr-secrets` Secret containing
`OLR_DATABASE_URL`, bootstrap/encryption credentials, and any OIDC/backend secrets.
You may keep bundled PostgreSQL enabled if you provide its required Secret keys
and manage its backups. The [chart reference](../charts/open-llm-router/README.md)
lists the Secret contract and external-database alternative.

The bundled Keycloak uses `start-dev` and a persistent H2 database for evaluation.
Use a production Keycloak service for public deployments. Register the portal
client, PKCE, groups mapper and redirect/logout URLs as described in
[external Keycloak setup](ADMIN_GUIDE.md). Set `keycloak.enabled: false` and supply
its `env.OIDC_*` settings and client secret. Local admin remains available.

Use an HTTPS `portal.baseURL`, `portal.cookieSecure: true`, an existing maintained
ingress controller, DNS and a TLS Secret. The production example includes F5 NGINX
streaming annotations; adapt them for your controller. Disable response buffering
and allow long read/send timeouts. Repeat the streaming check through the public
HTTPS endpoint before onboarding users. The chart does not install an ingress
controller, certificates or GPU workloads.

### Upgrade and rollback

Keep the release name, namespace, database identity and encryption key stable.
Back up PostgreSQL, Keycloak state, the credentials Secret and your private values
before an upgrade; see the [backup/restore runbook](RUNBOOK.md). Choose a tested
checkout, build its images, update their tags/digests, and apply the complete file:

```sh
helm upgrade --install olr charts/open-llm-router \
  -n olr -f "$OLR_SITE/values.yaml" --wait --timeout 10m
helm history olr -n olr
# Choose a revision from that history if a rollback is needed.
helm rollback olr PREVIOUS_REVISION -n olr --wait --timeout 10m
```

When upgrading from chart **0.1.x**, explicitly set `postgresql.enabled: false`,
`keycloak.enabled: false`, `demoBackend.enabled: false`, your original
`existingSecret`, and your full real model pool to preserve the external-service
setup. Enabling a bundled database does not migrate an external database.

Configuration changes update the router pod checksum. The router uses one
process and `Recreate` rollouts, so upgrades have a short availability gap.
Credential changes need an explicit workload restart. Database passwords and
Keycloak clients/users are initialized once: changing values does not rotate an
initialized database password or update an imported realm. Change them in the
service and Secret together. Changing portal origins later also requires updating
the Keycloak client's redirect and logout URLs in its console.

```sh
kubectl -n olr rollout restart deployment/olr-open-llm-router
```

`helm template` is an offline preview and cannot reuse installed generated
credentials. Use Helm install/upgrade against the cluster; do not apply an offline
render over an existing installation. Rollback restores chart resources, **not
PostgreSQL migrations/data, Keycloak state or Admin-saved endpoint revisions**.

```sh
helm uninstall olr -n olr
```

Uninstall removes workloads and Services but retains the chart-created credentials
Secret and both enabled PVCs. Reinstall the same release in the same namespace
with the same values to reuse the data and keys. Deleting those PVCs/Secret or the
namespace deletes that recovery path. Setting `persistence.enabled: false` uses
ephemeral storage; use it only for disposable demos.

The [Kustomize manifests](../deploy/k8s/base) remain an alternative; avoid having
Helm and Kustomize manage the same Deployment.

## Production Compose

For a server deployment with existing PostgreSQL, Keycloak and GPU pools, use the
standalone [pilot Compose walkthrough](PILOT.md#private-compose-deployment).
It covers private environment/config files, a tested image digest, HTTPS proxy
settings, startup checks and acceptance. Do not combine `pilot.yml` with the
development stack or use the development credentials on a public server.

## Troubleshooting

| Symptom | Practical check |
|---|---|
| Bundled database or Keycloak stays Pending | Check `kubectl -n olr get pvc` and `kubectl -n olr describe pod POD_NAME`; choose an available StorageClass and sufficient cluster memory. |
| Router never becomes ready | `docker compose logs --tail=100 router` or `kubectl -n olr logs deployment/olr-open-llm-router --tail=100`; check database URL/TLS, backend DNS/routing and health paths. Every configured model needs a healthy replica. |
| `ImagePullBackOff` / wrong architecture | Check the repository/tag/digest, registry pull Secret and that the image architecture matches the nodes. |
| `404 model not found` | Use `/v1/models` with a router key. Distinguish the public `chat-model` from the exact upstream model name. Use Admin model detection against every replica. |
| `403` on inference | The user's local memberships or Keycloak groups must match a configured model grant. Admin authority and model access are separate. |
| `503` on inference | Check replica health and capacity. A full single-GPU pool queues only for `queue_timeout_s`; increase it for long competing jobs or add capacity. |
| All output appears at once | Send `stream: true`, use `curl -N`, and disable proxy buffering. Check client streaming/display settings and allow time for model loading/prompt processing. |
| Login loops or cookies are refused | Match `portal.baseURL` / `PORTAL_BASE_URL` to the browser origin. Use HTTPS with secure cookies; explicitly disable them only for local HTTP. Check Keycloak redirect URLs. |
| Keycloak is unavailable | Use `/portal/login/local`. Check issuer discovery and client settings; local admin and API-key access work independently. |
| Changed YAML pool has no effect | A saved Admin endpoint revision takes precedence; restore to file configuration. YAML group/alias/OIDC changes need a process recreation. |
| Copy key is disabled | Issue a replacement for a hash-only key or restore the original encryption key. Do not rotate the encryption key casually; normal hashed-key authentication and key recovery are separate. |

Next: [administrator tasks](ADMIN_GUIDE.md), [client configuration](CLIENTS.md),
[operations and backups](RUNBOOK.md), [metrics/traces](OBSERVABILITY.md), and
[production pilot acceptance](PILOT.md).
