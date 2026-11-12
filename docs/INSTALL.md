# Installation

Use **Docker Compose** for a complete local environment with PostgreSQL, Keycloak
and mock backends. Use **Helm** to install the router on Kubernetes with your own
PostgreSQL and model servers. Neither method downloads or runs GPU models for you.

| Path | Includes | Start here |
|---|---|---|
| Local Compose | Router, persistent PostgreSQL, development Keycloak, two mock model servers | [Local Docker Compose](#local-docker-compose) |
| Kubernetes / Helm | Router, configuration, Service, optional Ingress | [Kubernetes with Helm](#kubernetes-with-helm) |
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

### 1. Prepare infrastructure and a router image

You need an accessible Kubernetes cluster, `kubectl` configured for its context,
[Helm 4](https://helm.sh/docs/intro/install/), Docker Buildx, a registry the nodes
can pull from, PostgreSQL 17, and a model backend reachable from the router pods.
The [chart](../charts/open-llm-router) installs the router, not PostgreSQL,
Keycloak, an ingress controller or GPU workloads. For public access, prepare DNS,
an existing maintained ingress controller, and a TLS certificate.

```sh
kubectl config current-context
kubectl cluster-info
helm version
export OLR_IMAGE_REPOSITORY=registry.example.org/olr
export OLR_IMAGE_TAG=$(git rev-parse --short=12 HEAD)
# Choose the cluster nodes' architecture, not necessarily your laptop's.
export OLR_IMAGE_PLATFORM=linux/amd64
docker login registry.example.org
docker buildx build --platform "$OLR_IMAGE_PLATFORM" --target olr \
  -t "$OLR_IMAGE_REPOSITORY:$OLR_IMAGE_TAG" --push .
```

Replace the registry; use `linux/arm64` for ARM nodes. No prebuilt public router
image or published Helm repository is assumed. For a private registry, provision
a pull Secret in namespace `olr` and set `imagePullSecrets: [{name: YOUR_SECRET}]`.

Create a dedicated database and a login role that owns it and can run schema
migrations. For self-managed PostgreSQL, an administrator can run these in `psql`:

```sql
CREATE ROLE olr LOGIN;
\password olr
CREATE DATABASE olr OWNER olr;
```

Use your provider's connection URL and TLS requirements; URI-encode special
characters in URL credentials. Confirm network policies/firewalls allow the pod
to reach PostgreSQL, the model pool and the Keycloak issuer if configured.

### 2. Create the namespace and private Secret

```sh
kubectl create namespace olr --dry-run=client -o yaml | kubectl apply -f -
umask 077
export OLR_SITE="$HOME/.config/olr-k8s"
mkdir -p "$OLR_SITE"
chmod 700 "$OLR_SITE"
cat > "$OLR_SITE/secrets.env" <<'EOF'
OLR_DATABASE_URL=REPLACE_WITH_POSTGRES_CONNECTION_URL
OLR_BOOTSTRAP_ADMIN_PASSWORD=REPLACE_WITH_PRIVATE_15_PLUS_CHARACTER_PASSWORD
OLR_API_KEY_ENCRYPTION_KEY=REPLACE_WITH_STABLE_BASE64_32_BYTE_KEY
EOF
chmod 600 "$OLR_SITE/secrets.env"
openssl rand -base64 32
```

Edit the file and replace every `REPLACE_…` value. Paste the generated encryption
key once and retain it across upgrades. `kubectl --from-env-file` treats quotes
as part of a value: write plain `NAME=value` lines, without surrounding quotes.
For external Keycloak, add `OIDC_PORTAL_CLIENT_SECRET`; group sync also needs
`OIDC_SYNC_CLIENT_SECRET`. Backend tokens and a dedicated `OLR_METRICS_TOKEN`
can be added here. Use your organization's secret manager instead if available.

```sh
kubectl -n olr create secret generic olr-secrets \
  --from-env-file="$OLR_SITE/secrets.env" --dry-run=client -o yaml \
  | kubectl apply -f -
```

Keep secrets out of Helm values and `--set`: Helm retains release values/history.
The chart references this existing Secret and never recreates its credentials.

### 3. Set model pools, login and network exposure

```sh
cp charts/open-llm-router/examples/production.yaml "$OLR_SITE/values.yaml"
```

Edit the copy:

- Set `image.repository` and `image.tag` to the image just pushed, or use
  `image.digest` to pin its tested `sha256:…` digest.
- Set `config.models.chat-model.upstream_model` and its complete `pool` list.
  The supplied example uses a LAN Ollama backend; replace it with your reachable
  origin. Keep Ollama's `/` health path, or use the backend's health endpoint.
- Set `config.groups` to your local/SSO memberships and desired access/quotas.
  Limits apply per user across all their keys. `env.OLR_BACKEND_ALLOWED_CIDRS`
  permits additional private destinations entered later through Admin.
- For Keycloak, set the `env.OIDC_*` values. Register confidential client
  `olr-portal` with PKCE S256, a groups mapper, redirect
  `https://llm.example.org/portal/callback` and post-logout redirect
  `https://llm.example.org/portal/`. Use your actual host. Group claim values must
  match `config.groups` exactly. See [external Keycloak setup](ADMIN_GUIDE.md).
  Remove the example's `OIDC_*` entries for local-only login.
- Set `portal.baseURL`, `ingress.host`, `ingress.className` and TLS Secret name.
  The example's annotations target **F5 NGINX Ingress Controller**, using
  [its documented streaming/timeout annotations](https://docs.nginx.com/nginx-ingress-controller/configuration/ingress-resources/advanced-configuration-with-annotations/).
  Use your controller's equivalent: response buffering off, 960-second read/send
  timeouts and a 32 MiB request body limit. Point DNS at that controller.

For an existing certificate/key on disk:

```sh
kubectl -n olr create secret tls olr-tls \
  --cert=/private/certs/fullchain.pem --key=/private/certs/privkey.pem \
  --dry-run=client -o yaml | kubectl apply -f -
```

An existing cert-manager Certificate may provision that Secret instead. If you
only want a private trial through port-forwarding, set these values instead of
configuring Ingress/TLS, and remove the external Keycloak `env.OIDC_*` entries:

```yaml
portal:
  baseURL: http://localhost:18080
  cookieSecure: false
ingress:
  enabled: false
```

The chart defaults to local-only authentication. With Keycloak configured, SSO
becomes the default choice and local login stays available during an IdP outage.
OIDC settings are deployment configuration; an Admin Keycloak editor is planned.

### 4. Install and verify

```sh
helm lint charts/open-llm-router -f "$OLR_SITE/values.yaml" --strict
helm template olr charts/open-llm-router -n olr -f "$OLR_SITE/values.yaml" \
  > "$OLR_SITE/rendered.yaml"
helm upgrade --install olr charts/open-llm-router \
  --namespace olr --create-namespace -f "$OLR_SITE/values.yaml" \
  --wait --timeout 5m
kubectl -n olr get pods,svc,ingress
kubectl -n olr rollout status deployment/olr-open-llm-router --timeout=300s
```

For a trial without Ingress, keep this running in another terminal:

```sh
kubectl -n olr port-forward service/olr-open-llm-router 18080:80
```

Verify through the address you configured:

```sh
export OLR_BASE_URL=https://llm.example.org
# For the port-forward trial instead: export OLR_BASE_URL=http://localhost:18080
curl --fail "$OLR_BASE_URL/healthz"
curl --fail "$OLR_BASE_URL/readyz"
curl --fail "$OLR_BASE_URL/portal/api/auth"
```

Open `$OLR_BASE_URL/portal/login/local`, sign in as `admin`, complete the required
password change, and sign in again. Create a local **staff** user, complete its
first login, and issue its API key as in the Compose walkthrough. Remove the
bootstrap password entry from your Secret source after changing it, reapply the
Secret and restart the Deployment. Retain the encryption key.

```sh
export OLR_API_KEY='PASTE_THE_ROUTER_KEY_HERE'
curl --fail "$OLR_BASE_URL/v1/models" \
  -H "Authorization: Bearer $OLR_API_KEY"
curl --fail -N "$OLR_BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $OLR_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[{"role":"user","content":"Hello"}],"stream":true}'
```

Expect incremental events and `[DONE]`; verify usage in the portal. Repeat through
the public HTTPS ingress before onboarding users: a successful port-forward
does not establish ingress streaming. Chart defaults enforce one replica and
`Recreate` rollouts, so upgrades have a short availability gap.

### Upgrade and rollback

1. Back up PostgreSQL, your private values and the stable encryption key. Record
   the current image digest and `helm history olr -n olr`.
2. Select a tested checkout, build/push its new image, and update the private
   `image.tag` or `image.digest`. Use the chart from that same checkout.
3. Apply the complete values file and repeat the health, login and stream checks:

```sh
helm upgrade --install olr charts/open-llm-router \
  -n olr -f "$OLR_SITE/values.yaml" --wait --timeout 5m
helm history olr -n olr
```

YAML changes update the pod checksum. A changed external Secret needs a restart
because environment variables are loaded at process creation:

```sh
kubectl -n olr rollout restart deployment/olr-open-llm-router
kubectl -n olr rollout status deployment/olr-open-llm-router --timeout=300s
```

To restore a previous Helm revision, choose its number from `helm history`:

```sh
helm rollback olr PREVIOUS_REVISION -n olr --wait --timeout 5m
```

Rollback restores chart-managed Kubernetes resources, **not PostgreSQL data,
database migrations, external Secrets or Admin-saved endpoint revisions**. The
router migrates at startup; check schema compatibility with the previous image
and follow the [backup/restore runbook](RUNBOOK.md) when a database restore is
needed. Admin-saved pools still override file/Helm configuration until restored
to file configuration in the endpoint editor.

```sh
# Removes the chart's router resources, preserving external PostgreSQL/Secrets.
helm uninstall olr -n olr
```

The existing [Kustomize manifests](../deploy/k8s/base) remain an alternative;
avoid having Helm and Kustomize manage the same Deployment. The older
`tests/k8s/acceptance.sh` targets community ingress-nginx, which
[retired in March 2026](https://kubernetes.io/blog/2026/03/30/kubernetes-v1-36-sneak-peek/).
The Helm example uses a separate maintained controller; adapt settings for your
cluster rather than installing that legacy acceptance controller.

## Production Compose

For a server deployment with existing PostgreSQL, Keycloak and GPU pools, use the
standalone [pilot Compose walkthrough](PILOT.md#private-compose-deployment).
It covers private environment/config files, a tested image digest, HTTPS proxy
settings, startup checks and acceptance. Do not combine `pilot.yml` with the
development stack or use the development credentials on a public server.

## Troubleshooting

| Symptom | Practical check |
|---|---|
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
