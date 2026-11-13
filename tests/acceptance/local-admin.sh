#!/usr/bin/env bash
# Fresh local-only install, isolated project/ports/volumes; no operator state.
set -euo pipefail
cd "$(dirname "$0")/../.."

task_project="olr-admin-acceptance-$$"
task_compose=(docker compose --env-file /dev/null -p "$task_project" -f deploy/compose/docker-compose.yml -f deploy/compose/local-only.yml)
read -r OLR_PORT POSTGRES_PORT MOCK_A_PORT MOCK_B_PORT KEYCLOAK_PORT < <(python3 - <<'PYPORTS'
import socket
sockets = [socket.socket() for _ in range(5)]
for sock in sockets:
    sock.bind(('127.0.0.1', 0))
print(*(sock.getsockname()[1] for sock in sockets))
for sock in sockets:
    sock.close()
PYPORTS
)
export OLR_PORT POSTGRES_PORT MOCK_A_PORT MOCK_B_PORT KEYCLOAK_PORT
export COMPOSE_PROJECT_NAME="$task_project"
export OLR_DATABASE_URL='postgres://olr:olr-dev-only@postgres:5432/olr?sslmode=disable'
export POSTGRES_USER=olr POSTGRES_DB=olr POSTGRES_PASSWORD=olr-dev-only
export OLR_BOOTSTRAP_ADMIN_PASSWORD='local-install-acceptance-test-only'
export OLR_BOOTSTRAP_ADMIN_PASSWORD_FILE=''
export OLR_SETTINGS_ENCRYPTION_KEY=''
export PORTAL_BASE_URL="http://localhost:$OLR_PORT" PORTAL_COOKIE_SECURE=false
export OLR_BASE_URL="$PORTAL_BASE_URL"
export COMPOSE_FILE="$(pwd)/deploy/compose/docker-compose.yml:$(pwd)/deploy/compose/local-only.yml"
cleanup() {
  task_exit=$?
  if test "$task_exit" -ne 0; then
    "${task_compose[@]}" logs --no-color --tail=20 router || true
  fi
  "${task_compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT
"${task_compose[@]}" config --quiet
"${task_compose[@]}" up --build --detach --wait --wait-timeout 90
export OLR_TEST_ROUTER_IMAGE
OLR_TEST_ROUTER_IMAGE="$(docker inspect --format '{{.Image}}' "$("${task_compose[@]}" ps -q router)")"
# A genuine no-IdP installation: the bundled Keycloak remains available in the
# standard development stack, but is neither started nor required in this mode.
if test -n "$("${task_compose[@]}" ps -q keycloak)"; then
  echo 'Unexpected identity provider in local-only acceptance stack' >&2
  exit 1
fi
python3 - <<'PYPROBE'
import json, os, urllib.request
base = os.environ['OLR_BASE_URL']
with urllib.request.urlopen(base + '/portal/api/auth', timeout=10) as response:
    methods = json.load(response)
assert methods['local']['available'] is True
assert methods['oidc']['configured'] is False
print('PASS: clean local-only install; local login available; no IdP configured')
PYPROBE
(
  cd web
  # These fixtures own disposable databases and routers inside this project's
  # network; none can reset the developer's or operator's existing administrator.
  npx playwright test tests/local-auth.spec.ts -g 'local-only first|regular local user|unreachable Keycloak|creates a local user|group defaults|administration history'
)
echo 'PASS: local administration acceptance (synthetic local evidence; not issue #21 pilot evidence)'
