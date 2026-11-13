#!/usr/bin/env bash
# Disposable kind cluster: real Helm install, API/local login, upgrade/rollback.
# Needs Docker, kind, Helm 4, kubectl and Python 3; never uses the caller's cluster.
set -euo pipefail
cd "$(dirname "$0")/../.."
helm_bin=${HELM:-helm}
kind_bin=${KIND:-kind}
cluster=olr-helm-$$
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
pf=
kc_pf=
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    kubectl -n olr get pods --request-timeout=10s || true
    for app in deployment/olr-open-llm-router deployment/olr-open-llm-router-keycloak statefulset/olr-open-llm-router-postgresql; do
      kubectl -n olr logs "$app" --all-containers --tail=80 --request-timeout=10s || true
    done
  fi
  stop_forward
  if [ -n "$kc_pf" ]; then kill "$kc_pf" 2>/dev/null || true; wait "$kc_pf" 2>/dev/null || true; fi
  "$kind_bin" delete cluster --name "$cluster" || true
  rm -rf "$work"
}
trap cleanup EXIT
stop_forward() {
  if [ -n "$pf" ]; then
    kill "$pf" 2>/dev/null || true
    wait "$pf" 2>/dev/null || true
    pf=
  fi
}
"$kind_bin" create cluster --name "$cluster" --kubeconfig "$KUBECONFIG" --wait 120s
docker build --target olr -t olr-router:helm-test .
docker build --target mock-llm -t olr-mock-llm:helm-test .
"$kind_bin" load docker-image olr-router:helm-test olr-mock-llm:helm-test --name "$cluster"
kubectl create namespace olr
kubectl label namespace olr pod-security.kubernetes.io/enforce=restricted
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
base=http://localhost:$port
kc_port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
cat > "$work/values.yaml" <<EOF
image:
  repository: olr-router
  tag: helm-test
demoBackend:
  image:
    repository: olr-mock-llm
    tag: helm-test
portal:
  baseURL: $base
keycloak:
  baseURL: http://localhost:$kc_port
config:
  models:
    chat-model:
      # Probe offline inference without the optional 120-second capacity queue.
      queue_timeout_s: 0
EOF
install() {
  "$helm_bin" upgrade --install olr charts/open-llm-router -n olr \
    -f "$work/values.yaml" --wait --timeout 300s "$@"
}
forward() {
  stop_forward
  kubectl -n olr port-forward service/olr-open-llm-router "$port:80" > "$work/forward.log" 2>&1 &
  pf=$!
  for attempt in $(seq 1 30); do
    if curl --fail --silent --max-time 2 "$base/readyz" > /dev/null; then return; fi
    sleep 1
  done
  cat "$work/forward.log"
  return 1
}
checksum() {
  kubectl -n olr get deployment olr-open-llm-router -o 'jsonpath={.spec.template.metadata.annotations.checksum/config}'
}
models() {
  curl --fail --silent --show-error "$base/v1/models" -H "Authorization: Bearer $key" > "$work/models.json"
  python3 -c 'import json,sys; assert any(m["id"]=="chat-model" for m in json.load(open(sys.argv[1]))["data"])' "$work/models.json"
}
# No database, Keycloak, mocks or Secrets are provisioned outside Helm.
install
kubectl -n olr get secret olr-open-llm-router-credentials -o json > "$work/initial-secret.json"
secret_data() {
  kubectl -n olr get secret olr-open-llm-router-credentials -o 'jsonpath={.data}'
}
first_secret=$(secret_data)
first_checksum=$(checksum)
forward
# Complete first local administrator login without putting a password in argv.
python3 - "$work" <<'PYLOGIN'
import base64, json, pathlib, sys
work = pathlib.Path(sys.argv[1])
data = json.loads((work / "initial-secret.json").read_text())["data"]
password = base64.b64decode(data["OLR_BOOTSTRAP_ADMIN_PASSWORD"]).decode()
(work / "login.json").write_text(json.dumps({"username": "admin", "password": password}))
(work / "change.json").write_text(json.dumps({"current_password": password, "new_password": "helm-only-replacement-password"}))
PYLOGIN
curl --fail --silent --show-error -c "$work/cookies" -H "Origin: $base" -H 'Content-Type: application/json' \
  --data-binary @"$work/login.json" "$base/portal/login/local" > "$work/login-response.json"
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["must_change_password"]' "$work/login-response.json"
curl --fail --silent --show-error -b "$work/cookies" -H "Origin: $base" -H 'Content-Type: application/json' \
  --data-binary @"$work/change.json" "$base/portal/password" > /dev/null
cat > "$work/login.json" <<'EOF'
{"username":"admin","password":"helm-only-replacement-password"}
EOF
curl --fail --silent --show-error -c "$work/cookies" -H "Origin: $base" -H 'Content-Type: application/json' \
  --data-binary @"$work/login.json" "$base/portal/login/local" > /dev/null
curl --fail --silent --show-error -b "$work/cookies" "$base/portal/api/me" > "$work/me.json"
python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); assert m["admin"] and m["user"]["protected_admin"]' "$work/me.json"
# Test the bundled realm through real browser-style authorization-code login.
kubectl -n olr port-forward service/olr-open-llm-router-keycloak "$kc_port:8080" > "$work/keycloak-forward.log" 2>&1 &
kc_pf=$!
for attempt in $(seq 1 30); do
  if curl --fail --silent --max-time 2 "http://localhost:$kc_port/realms/olr-demo/.well-known/openid-configuration" > /dev/null; then break; fi
  sleep 1
done
sso() {
  python3 - "$base" "$work/initial-secret.json" <<'PYSSO'
import base64, json, sys
sys.path.insert(0, "tests/clients")
from check_portal import Browser
password = base64.b64decode(json.load(open(sys.argv[2]))["data"]["OLR_DEMO_USER_PASSWORD"]).decode()
browser = Browser(sys.argv[1])
me = browser.login("staff", password)
assert me["user"]["groups"] == ["staff"]
key = browser.request("/portal/api/keys", "POST", {"label": "helm-sso"}, want=201)["key"]
browser.request("/v1/models", key=key)
browser.logout()
PYSSO
}
sso
user=$(kubectl -n olr exec deployment/olr-open-llm-router -- olr admin user add -username helm-test -groups staff)
key=$(kubectl -n olr exec deployment/olr-open-llm-router -- olr admin key create -user "$user" -label helm-test)
models
curl --fail --silent --show-error -N "$base/v1/chat/completions" \
  -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[],"stream":true}' > "$work/stream.sse"
python3 -c 'import sys; assert "data: [DONE]" in open(sys.argv[1]).read()' "$work/stream.sse"
# A removed extra token must not be retained from the previous Helm values.
install --set secrets.env.HELM_EXTRA_TOKEN=test-only
install --set config.models.chat-model.idle_timeout_s=121
[ "$(secret_data)" = "$first_secret" ]
[ "$(checksum)" != "$first_checksum" ]
forward
models
"$helm_bin" rollback olr 1 -n olr --wait --timeout 300s
[ "$(checksum)" = "$first_checksum" ]
[ "$(secret_data)" = "$first_secret" ]
forward
models
"$helm_bin" uninstall olr -n olr --wait --timeout 300s
# Storage and credentials survive uninstall, workloads do not.
[ "$(secret_data)" = "$first_secret" ]
kubectl -n olr get pvc olr-open-llm-router-postgresql olr-open-llm-router-keycloak > /dev/null
[ -z "$(kubectl -n olr get deployment,statefulset,service -o name)" ]
install
[ "$(secret_data)" = "$first_secret" ]
forward
models
# The SSO password/realm survive reinstall alongside the router's API key.
kill "$kc_pf" 2>/dev/null || true
wait "$kc_pf" 2>/dev/null || true
kubectl -n olr port-forward service/olr-open-llm-router-keycloak "$kc_port:8080" > "$work/keycloak-forward.log" 2>&1 &
kc_pf=$!
for attempt in $(seq 1 30); do
  if curl --fail --silent --max-time 2 "http://localhost:$kc_port/realms/olr-demo/.well-known/openid-configuration" > /dev/null; then break; fi
  sleep 1
done
sso
# The router Service must remain ready when its only inference backend is gone.
kubectl -n olr scale deployment/olr-open-llm-router-demo --replicas=0
for attempt in $(seq 1 30); do
  code=$(curl --silent --show-error --max-time 3 -o "$work/offline.json" -w '%{http_code}' \
    -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
    -d '{"model":"chat-model","messages":[],"max_tokens":1}' "$base/v1/chat/completions")
  if [ "$code" = 503 ]; then break; fi
  [ "$attempt" != 30 ] || { echo "offline inference returned $code, expected 503"; exit 1; }
  sleep 1
done
# Cover at least one Kubernetes readiness probe after the backend becomes unhealthy.
sleep 6
kubectl -n olr wait --for=condition=Ready pod -l app.kubernetes.io/name=open-llm-router --timeout=30s
curl --fail --silent --show-error "$base/readyz" > /dev/null
curl --fail --silent --show-error "$base/portal/login/local" > /dev/null
models
# Disable the demo entirely; no OLR_DEMO_BACKEND_URL or model pool is required.
install --set demoBackend.enabled=false
forward
curl --fail --silent --show-error "$base/portal/login/local" > /dev/null
curl --fail --silent --show-error "$base/v1/models" -H "Authorization: Bearer $key" > "$work/empty-models.json"
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["data"] == []' "$work/empty-models.json"
install
forward
models
"$helm_bin" uninstall olr -n olr --wait --timeout 300s
echo 'Helm acceptance passed: full-stack install, local admin, SSO, streaming, upgrade/rollback, persistent reinstall, backend outage and demo-disabled setup.'
