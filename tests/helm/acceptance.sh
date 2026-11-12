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
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    kubectl -n olr get pods --request-timeout=10s || true
    kubectl -n olr logs deployment/olr-open-llm-router --tail=80 --request-timeout=10s || true
  fi
  stop_forward
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
kubectl -n olr apply -f deploy/k8s/test/postgres.yaml
sed 's/image: mock-llm/image: olr-mock-llm:helm-test/' deploy/k8s/test/mocks.yaml > "$work/mocks.yaml"
kubectl -n olr apply -f "$work/mocks.yaml"
kubectl -n olr rollout status statefulset/postgres --timeout=180s
cat > "$work/secrets.env" <<'EOF'
OLR_DATABASE_URL=postgres://olr:olr-k8s-test-only@postgres:5432/olr?sslmode=disable
OLR_BOOTSTRAP_ADMIN_PASSWORD=helm-only-bootstrap-password
OLR_API_KEY_ENCRYPTION_KEY=BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc=
EOF
kubectl -n olr create secret generic olr-secrets --from-env-file="$work/secrets.env"
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
base=http://localhost:$port
cat > "$work/values.yaml" <<EOF
image:
  repository: olr-router
  tag: helm-test
portal:
  baseURL: $base
  cookieSecure: false
config:
  models:
    chat-model:
      upstream_model: chat-model
      pool:
        - {url: http://mock-llm-a:8000, health_path: /health, weight: 1, max_inflight: 4}
EOF
install() {
  "$helm_bin" upgrade --install olr charts/open-llm-router -n olr \
    -f "$work/values.yaml" --wait --timeout 180s "$@"
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
install
first_checksum=$(checksum)
forward
# Complete first local administrator login without putting a password in argv.
cat > "$work/login.json" <<'EOF'
{"username":"admin","password":"helm-only-bootstrap-password"}
EOF
curl --fail --silent --show-error -c "$work/cookies" -H "Origin: $base" -H 'Content-Type: application/json' \
  --data-binary @"$work/login.json" "$base/portal/login/local" > "$work/login-response.json"
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["must_change_password"]' "$work/login-response.json"
cat > "$work/change.json" <<'EOF'
{"current_password":"helm-only-bootstrap-password","new_password":"helm-only-replacement-password"}
EOF
curl --fail --silent --show-error -b "$work/cookies" -H "Origin: $base" -H 'Content-Type: application/json' \
  --data-binary @"$work/change.json" "$base/portal/password" > /dev/null
cat > "$work/login.json" <<'EOF'
{"username":"admin","password":"helm-only-replacement-password"}
EOF
curl --fail --silent --show-error -c "$work/cookies" -H "Origin: $base" -H 'Content-Type: application/json' \
  --data-binary @"$work/login.json" "$base/portal/login/local" > /dev/null
curl --fail --silent --show-error -b "$work/cookies" "$base/portal/api/me" > "$work/me.json"
python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); assert m["admin"] and m["user"]["protected_admin"]' "$work/me.json"
user=$(kubectl -n olr exec deployment/olr-open-llm-router -- olr admin user add -username helm-test -groups staff)
key=$(kubectl -n olr exec deployment/olr-open-llm-router -- olr admin key create -user "$user" -label helm-test)
models
curl --fail --silent --show-error -N "$base/v1/chat/completions" \
  -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
  -d '{"model":"chat-model","messages":[],"stream":true}' > "$work/stream.sse"
python3 -c 'import sys; assert "data: [DONE]" in open(sys.argv[1]).read()' "$work/stream.sse"
install --set config.models.chat-model.idle_timeout_s=121
[ "$(checksum)" != "$first_checksum" ]
forward
models
"$helm_bin" rollback olr 1 -n olr --wait --timeout 180s
[ "$(checksum)" = "$first_checksum" ]
forward
models
"$helm_bin" uninstall olr -n olr --wait --timeout 180s
kubectl -n olr get secret olr-secrets > /dev/null
kubectl -n olr get statefulset postgres > /dev/null
echo 'Helm acceptance passed: install, local admin, streaming, upgrade, rollback and uninstall.'
