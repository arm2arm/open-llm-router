#!/usr/bin/env bash
set -euo pipefail
if [[ $# != 4 ]]; then
  echo "usage: $0 RELEASE NAMESPACE VALUES.yaml IMAGE-POLICY.json" >&2
  exit 2
fi
root=$(cd "$(dirname "$0")/../.." && pwd)
export OLR_IMAGE_POLICY
OLR_IMAGE_POLICY=$(cd "$(dirname "$4")" && pwd)/$(basename "$4")
exec helm upgrade --install "$1" "$root/charts/open-llm-router" \
  --namespace "$2" --values "$3" --set production=true \
  --post-renderer "$root/deploy/release/verify_images.py" --wait --timeout 5m
