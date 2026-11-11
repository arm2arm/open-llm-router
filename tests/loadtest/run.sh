#!/usr/bin/env bash
# Synthetic load in an ephemeral Compose project. Never touches the dev stack.
set -euo pipefail
cd "$(dirname "$0")/../.."
project="olr-loadtest-${UID:-0}-$$"
available_cpus=$(docker info --format '{{.NCPU}}')
if [ "$available_cpus" -lt 4 ]; then export OLR_LOADTEST_CPUS="$available_cpus"; fi
if [ "$available_cpus" -lt 2 ]; then export OLR_LOADTEST_AUX_CPUS="$available_cpus"; fi
compose=(docker compose --env-file /dev/null -f deploy/compose/loadtest.yml -p "$project")
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    # Router logs contain metadata only. Never dump mock /_mock/stats headers.
    "${compose[@]}" logs --no-color --tail 15 router >&2 || true
  fi
  "${compose[@]}" down --volumes --remove-orphans >&2 || true
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" build router mock-llm-a mock-llm-b loadtest >&2
"${compose[@]}" up --detach --no-build --wait --wait-timeout 60 router >&2
user_id=$("${compose[@]}" exec -T router olr admin user add -username loadtest -groups loadtest)
OLR_API_KEY=$("${compose[@]}" exec -T router olr admin key create -user "$user_id" -label loadtest)
export OLR_API_KEY
"${compose[@]}" run --rm --no-deps -T loadtest \
  -router http://router:8080 \
  -direct http://mock-llm-a:8000,http://mock-llm-b:8000 \
  -weights 2,1 -mock "$@"
