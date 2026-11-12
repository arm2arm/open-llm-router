#!/usr/bin/env bash
set -euo pipefail
if [[ $# != 4 ]]; then
  echo "usage: $0 RELEASE NAMESPACE VALUES.yaml IMAGE-POLICY.json" >&2
  exit 2
fi
root=$(cd "$(dirname "$0")/../.." && pwd)
export OLR_IMAGE_POLICY
OLR_IMAGE_POLICY=$(cd "$(dirname "$4")" && pwd)/$(basename "$4")
# Isolate the Helm 4 plugin registry to this checkout's verification code.
HELM_PLUGINS=$(mktemp -d)
export HELM_PLUGINS
trap 'rm -rf "$HELM_PLUGINS"' EXIT
ln -s "$root/deploy/release" "$HELM_PLUGINS/olr-verify-images"
helm upgrade --install "$1" "$root/charts/open-llm-router" \
  --namespace "$2" --values "$3" --set production=true \
  --post-renderer olr-verify-images --wait --timeout 5m
