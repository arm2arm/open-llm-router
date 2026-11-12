#!/usr/bin/env bash
# Disposable policy-enforcing kind cluster. Never uses the caller's kubeconfig.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d)
cluster=olr-policy-$$
export KUBECONFIG=$work/kubeconfig
kind_bin=${KIND:-kind}
helm_bin=${HELM:-helm}
python_bin=${PYTHON:-python3}
cleanup() { "$kind_bin" delete cluster --name "$cluster"; rm -rf "$work"; }
trap cleanup EXIT
cat > "$work/kind.yaml" <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
YAML
"$kind_bin" create cluster --name "$cluster" --config "$work/kind.yaml"
# Calico's official versioned manifest supplies actual policy enforcement.
curl -fsSL https://raw.githubusercontent.com/projectcalico/calico/v3.33.0/manifests/calico.yaml -o "$work/calico.yaml"
kubectl create -f "$work/calico.yaml"
kubectl -n kube-system rollout status ds/calico-node --timeout=180s
kubectl wait --for=condition=Ready nodes --all --timeout=180s
for ns in olr ingress-nginx; do kubectl create namespace "$ns"; done
probe_image=postgres:17
for format in helm kustomize; do
  if [ "$format" = helm ]; then
    "$helm_bin" template olr charts/open-llm-router -n olr > "$work/rendered.yaml"
  else
    kubectl kustomize deploy/k8s/test > "$work/rendered.yaml"
  fi
  "$python_bin" - "$work/rendered.yaml" "$work/policy.yaml" <<'PY'
import sys,yaml
items=[x for x in yaml.safe_load_all(open(sys.argv[1])) if x and x['kind']=='NetworkPolicy']
for item in items: item['metadata']['namespace']='olr'
open(sys.argv[2],'w').write(yaml.safe_dump_all(items))
PY
  kubectl apply -f "$work/policy.yaml"
  if [ "$format" = helm ]; then
    router='app.kubernetes.io/name=open-llm-router,app.kubernetes.io/instance=olr'
    db='app.kubernetes.io/name=open-llm-router-postgresql,app.kubernetes.io/instance=olr,app.kubernetes.io/component=postgresql'
    backend='app.kubernetes.io/name=open-llm-router-demo,app.kubernetes.io/instance=olr,app.kubernetes.io/component=demo'
  else
    router='app.kubernetes.io/component=router'
    db='app.kubernetes.io/component=postgres'
    backend='app.kubernetes.io/component=mock-llm-a'
  fi
  kubectl -n olr run db --image="$probe_image" --labels="$db" --env=POSTGRES_PASSWORD=policy-test-only
  kubectl -n olr run backend --image=busybox:1.37.0 --labels="$backend" --command -- httpd -f -p 8000
  kubectl -n olr run router --image="$probe_image" --labels="$router" --dry-run=client -o json --command -- sleep 600 > "$work/router.json"
  "$python_bin" - "$work/router.json" <<'PYROUTER'
import json,sys
p=sys.argv[1];d=json.load(open(p));d['spec']['containers'].append({'name':'http','image':'busybox:1.37.0','command':['httpd','-f','-p','8080']});json.dump(d,open(p,'w'))
PYROUTER
  kubectl apply -f "$work/router.json"
  kubectl -n olr run unrelated --image="$probe_image" --command -- sleep 600
  kubectl -n ingress-nginx run controller --image="$probe_image" --labels=app.kubernetes.io/component=controller --command -- sleep 600
  kubectl -n olr wait --for=condition=Ready pod --all --timeout=180s
  kubectl -n ingress-nginx wait --for=condition=Ready pod/controller --timeout=180s
  db_ip=$(kubectl -n olr get pod db -o jsonpath='{.status.podIP}')
  router_ip=$(kubectl -n olr get pod router -o jsonpath='{.status.podIP}')
  backend_ip=$(kubectl -n olr get pod backend -o jsonpath='{.status.podIP}')
  # Wait for postgres and asynchronous policy programming before negative checks.
  for attempt in $(seq 1 30); do
    if kubectl -n olr exec router -- pg_isready -h "$db_ip" -t 2; then break; fi
    [ "$attempt" != 30 ] || exit 1
    sleep 2
  done
  sleep 5
  kubectl -n olr exec router -- getent hosts kubernetes.default.svc.cluster.local
  kubectl -n olr exec router -- timeout 3 bash -c "exec 3<>/dev/tcp/$backend_ip/8000"
  kubectl -n ingress-nginx exec controller -- timeout 3 bash -c "exec 3<>/dev/tcp/$router_ip/8080"
  if kubectl -n olr exec unrelated -- timeout 3 bash -c "exec 3<>/dev/tcp/$router_ip/8080"; then echo 'unrelated pod reached router/admin listener'; exit 1; fi
  if kubectl -n olr exec unrelated -- pg_isready -h "$db_ip" -t 2; then echo 'unrelated pod reached database'; exit 1; fi
  if kubectl -n olr exec unrelated -- timeout 3 bash -c "exec 3<>/dev/tcp/$backend_ip/8000"; then echo 'unrelated pod reached backend'; exit 1; fi
  if kubectl -n ingress-nginx exec controller -- timeout 3 bash -c "exec 3<>/dev/tcp/$backend_ip/8000"; then echo 'ingress controller reached private backend'; exit 1; fi
  kubectl -n olr delete pods --all --grace-period=0 --force --wait=true
  kubectl -n ingress-nginx delete pod controller --grace-period=0 --force --wait=true
  kubectl -n olr delete networkpolicy --all
  echo "$format: router database/backend and DNS allowed; unrelated access denied"
done
