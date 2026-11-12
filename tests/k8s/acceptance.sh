#!/usr/bin/env bash
# Deploy deploy/k8s/test into a disposable kind cluster behind ingress-nginx
# and run the Compose acceptance checks through the Ingress. Needs Docker,
# kubectl, kind (go install sigs.k8s.io/kind@v0.27.0) and uv.
# KEEP=1 leaves the cluster running.
set -euo pipefail
cd "$(dirname "$0")/../.."
cluster=olr-k8s-test
port=${OLR_K8S_PORT:-18480}
work=$(mktemp -d)
export KUBECONFIG=$work/kubeconfig
cleanup() {
	[ -n "${pf:-}" ] && kill $pf 2>/dev/null || true
	[ "${KEEP:-0}" = 1 ] || kind delete cluster --name $cluster >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

cat >"$work/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "ingress-ready=true"
    extraPortMappings:
      - {containerPort: 80, hostPort: $port, listenAddress: 127.0.0.1, protocol: TCP}
YAML
kind create cluster --name $cluster --config "$work/kind.yaml"
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.12.1/deploy/static/provider/kind/deploy.yaml
docker build -q --target olr -t olr-router:k8s-test .
docker build -q --target mock-llm -t olr-mock-llm:k8s-test .
kind load docker-image olr-router:k8s-test olr-mock-llm:k8s-test --name $cluster
kubectl -n ingress-nginx wait --for=condition=available deploy/ingress-nginx-controller --timeout=300s
kubectl -n ingress-nginx wait --for=condition=complete job/ingress-nginx-admission-patch --timeout=300s
# The controller's admission webhook may refuse connections for a while after
# the Deployment reports available.
for attempt in $(seq 1 30); do
	kubectl apply -k deploy/k8s/test && break
	[ "$attempt" = 30 ] && exit 1
	sleep 5
done
kubectl -n olr rollout status deploy/olr-router --timeout=300s

base=http://localhost:$port
olr() { kubectl -n olr exec deploy/olr-router -- olr "$@"; }
curl --fail --silent --retry 30 --retry-all-errors --retry-delay 2 "$base/readyz" >/dev/null
user=$(olr admin user add -username k8s-sdk -groups staff)
key=$(olr admin key create -user "$user" -label sdk)
kubectl -n olr port-forward svc/mock-llm-a 18491:8000 >/dev/null & pf=$!
kubectl -n olr port-forward svc/mock-llm-b 18492:8000 >/dev/null & pf="$pf $!"
sleep 3
stats=http://127.0.0.1:18491/_mock/stats,http://127.0.0.1:18492/_mock/stats
common=(OLR_BASE_URL=$base OLR_API_KEY=$key OLR_TEST_MOCK=1 OLR_CHECK_EXTENDED=1 OLR_REQUIRE_CANCEL_EVIDENCE=1
	OLR_CANCEL_STATS_URLS=$stats OLR_VLLM_METRICS_URL= OLR_VLLM_LOG_PATH=)
env "${common[@]}" OLR_EMBEDDING_MODEL=chat-model uv run tests/clients/check_openai.py
env "${common[@]}" OLR_CHECK_THINKING=1 uv run tests/clients/check_anthropic.py
limited=$(olr admin user add -username k8s-limits -groups students)
OLR_BASE_URL=$base OLR_API_KEY="$(olr admin key create -user "$limited" -label limits)" uv run tests/clients/check_limits.py

# Streaming through the Ingress: incremental, and longer than nginx's default 60 s.
start=$(date +%s)
curl -sN --max-time 150 "$base/v1/chat/completions" -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
	-H 'X-Mock-Tokens: 25' -H 'X-Mock-Token-Delay: 3s' -d '{"model":"chat-model","messages":[],"stream":true}' >"$work/long.sse"
elapsed=$(($(date +%s) - start))
grep -q 'data: \[DONE\]' "$work/long.sse" && [ "$elapsed" -ge 70 ] || { echo "long stream failed after ${elapsed}s"; exit 1; }
first=$( { curl -sN "$base/v1/chat/completions" -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
	-H 'X-Mock-Tokens: 50' -H 'X-Mock-Token-Delay: 100ms' -w '%{time_starttransfer}' -o /dev/null \
	-d '{"model":"chat-model","messages":[],"stream":true}'; } )
awk -v t="$first" 'BEGIN { exit !(t < 1.0) }' || { echo "first byte after ${first}s: the Ingress buffers"; exit 1; }
echo "Kubernetes acceptance passed: SDKs, cancellation, limits, ${elapsed}s stream, first byte ${first}s"
