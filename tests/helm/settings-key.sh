#!/usr/bin/env bash
# Exercise Secret lookup against a real disposable Kubernetes API, without LLMs.
set -euo pipefail
cd "$(dirname "$0")/../.."
work=$(mktemp -d)
cluster=olr-key-$$
export KUBECONFIG=$work/kubeconfig
cleanup() {
  result=$?
  if [ "$result" -ne 0 ] && [ -f "$work/upgrade.log" ]; then cat "$work/upgrade.log" >&2; fi
  kind delete cluster --name "$cluster" || true
  rm -rf "$work"
}
trap cleanup EXIT
kind create cluster --name "$cluster" --kubeconfig "$KUBECONFIG" --wait 120s
cat > "$work/site.yaml" <<'YAML'
postgresql:
  enabled: false
externalDatabase:
  url: postgres://test:test@unused.invalid/test
keycloak:
  enabled: false
demoBackend:
  enabled: false
YAML
upgrade() {
  helm upgrade --install olr charts/open-llm-router -n olr --create-namespace \
    -f "$work/site.yaml" "$@" > "$work/upgrade.log" 2>&1
}
stored() {
  kubectl -n olr get secret olr-open-llm-router-credentials \
    -o jsonpath='{.data.OLR_SETTINGS_ENCRYPTION_KEY}' | base64 -d
}
key=$(printf '01234567890123456789012345678901' | base64 | tr -d '\n')
other=$(printf 'abcdefghijklmnopqrstuvwxyzABCDEF' | base64 | tr -d '\n')
upgrade --set-string "secrets.env.OLR_SETTINGS_ENCRYPTION_KEY=$key" --set-string secrets.env.TEST_EXTRA_TOKEN=synthetic
[ "$(stored)" = "$key" ]
# Missing and empty values both retain the key; ordinary tokens are removed.
upgrade
[ "$(stored)" = "$key" ]
[ -z "$(kubectl -n olr get secret olr-open-llm-router-credentials -o jsonpath='{.data.TEST_EXTRA_TOKEN}')" ]
upgrade --set-string secrets.env.OLR_SETTINGS_ENCRYPTION_KEY=
[ "$(stored)" = "$key" ]
if upgrade --set-string "secrets.env.OLR_SETTINGS_ENCRYPTION_KEY=$other"; then
  echo 'A different encryption key was incorrectly accepted' >&2
  exit 1
fi
grep -q 'cannot change an existing settings encryption key' "$work/upgrade.log"
[ "$(stored)" = "$key" ]
# The guarded key is also preserved when reinstalling after Helm uninstall.
helm uninstall olr -n olr > "$work/uninstall.log" 2>&1
upgrade
[ "$(stored)" = "$key" ]
echo 'Settings key upgrade/reinstall preservation, rotation rejection and ordinary token removal passed.'
