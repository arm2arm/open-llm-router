#!/usr/bin/env bash
set -euo pipefail
if [[ $# != 2 ]]; then
  echo "usage: $0 KUSTOMIZE_DIRECTORY IMAGE-POLICY.json" >&2
  exit 2
fi
root=$(cd "$(dirname "$0")/../.." && pwd)
manifest=$(mktemp)
trap 'rm -f "$manifest"' EXIT
# No YAML reaches kubectl apply until the complete render passes verification.
kubectl kustomize "$1" | "$root/deploy/release/verify_images.py" --policy "$2" > "$manifest"
kubectl apply -f "$manifest"
