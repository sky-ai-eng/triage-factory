#!/usr/bin/env bash
# Runs the SSO end-to-end suite: the real multi-mode stack (Postgres, GoTrue,
# SeaweedFS, and the host-built control binary) signing in through two test
# SAML identity providers and a fake GitHub, driven by Playwright. Every key,
# certificate and secret is generated into e2e/sso/.state/ for this run and
# nothing outside it is written except the compose project's containers.
#
#   e2e/sso/run.sh                      build, bring up, test, tear down
#   SSO_E2E_KEEP_UP=1 e2e/sso/run.sh    leave the stack running afterwards
#   SSO_E2E_SKIP_FRONTEND=1 ...         reuse an existing frontend/dist
#
# Exits with Playwright's status. See e2e/sso/README.md.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
SUITE="$ROOT/e2e/sso"
STATE="$SUITE/.state"
DEV="$ROOT/scripts/multi-mode-dev.sh"
BIN="$STATE/triagefactory"

log() { printf '\n==> %s\n' "$*"; }

# 1. Every port the stack and the test servers bind must be free: a stale
# process on one of them would answer in place of the one under test.
busy=()
for port in 3000 5432 8333 8443 8444 8445 9999; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    busy+=("$port")
  fi
done
if [ ${#busy[@]} -gt 0 ]; then
  echo "ports in use: ${busy[*]} — stop whatever holds them (a running multi-mode-dev stack: scripts/multi-mode-dev.sh down)" >&2
  exit 1
fi

# 2. Cloud sessions do not start the Docker daemon.
if ! docker info >/dev/null 2>&1; then
  if [ "$(uname -s)" != Linux ]; then
    echo "docker is not running — start Docker Desktop and retry" >&2
    exit 1
  fi
  log "starting dockerd"
  sudo_cmd=()
  [ "$(id -u)" = 0 ] || sudo_cmd=(sudo)
  "${sudo_cmd[@]}" sh -c 'nohup dockerd >/var/log/dockerd.log 2>&1 &'
  for _ in $(seq 1 60); do
    docker info >/dev/null 2>&1 && break
    sleep 1
  done
  docker info >/dev/null 2>&1 || { echo "dockerd did not come up" >&2; exit 1; }
fi

rm -rf "$STATE"
mkdir -p "$STATE"

FAKES_PID=""
TF_PID=""
cleanup() {
  local status=$?
  if [ "${SSO_E2E_KEEP_UP:-}" = 1 ]; then
    log "SSO_E2E_KEEP_UP=1: leaving the stack up (TF pid ${TF_PID:-none}, fakes pid ${FAKES_PID:-none})"
    exit "$status"
  fi
  log "tearing down"
  [ -n "$TF_PID" ] && kill "$TF_PID" 2>/dev/null && wait "$TF_PID" 2>/dev/null
  # GoTrue's log explains a refused assertion or exchange; the containers go
  # away below.
  gotrue=$(docker ps -aq --filter label=com.docker.compose.project=tf-multi-dev --filter label=com.docker.compose.service=gotrue | head -1)
  [ -n "$gotrue" ] && docker logs "$gotrue" >"$STATE/gotrue.log" 2>&1
  MULTI_DEV_ENV_FILE="$STATE/env" MULTI_DEV_COMPOSE_OVERLAY="$SUITE/compose.e2e.yml" "$DEV" down || true
  [ -n "$FAKES_PID" ] && kill "$FAKES_PID" 2>/dev/null && wait "$FAKES_PID" 2>/dev/null
  exit "$status"
}
trap cleanup EXIT

# 3. The test servers.
log "starting the test identity providers and fake GitHub"
GH_CLIENT_ID=sso-e2e
GH_CLIENT_SECRET=$(openssl rand -hex 16)
(cd "$ROOT/e2e" && go build -o "$STATE/fakes" ./sso/fakes)
"$STATE/fakes" serve -state "$STATE" -users "$SUITE/fixtures/users.json" \
  -github-client-id "$GH_CLIENT_ID" -github-client-secret "$GH_CLIENT_SECRET" \
  >"$STATE/fakes.log" 2>&1 &
FAKES_PID=$!
fakes_curl() {
  curl -fsS --cacert "$STATE/ca.pem" \
    --resolve idp-globex.test:8443:127.0.0.1 \
    --resolve idp-mallory.test:8444:127.0.0.1 \
    --resolve github.test:8445:127.0.0.1 "$@"
}
for _ in $(seq 1 30); do
  fakes_curl -o /dev/null https://idp-globex.test:8443/metadata 2>/dev/null && break
  sleep 1
done
fakes_curl -o /dev/null https://idp-globex.test:8443/metadata

# 4. The run's licence, SAML key and env file.
log "generating the licence, SAML key and env file"
TF_LICENSE=$("$STATE/fakes" license -state "$STATE")
GOTRUE_SAML_PRIVATE_KEY=$(openssl genpkey -algorithm rsa -pkeyopt rsa_keygen_bits:2048 -outform DER 2>/dev/null | base64 | tr -d '\n')
umask 077
cat >"$STATE/env" <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 32)
SUPABASE_AUTH_ADMIN_PASSWORD=$(openssl rand -hex 32)
TF_AUTHENTICATOR_PASSWORD=$(openssl rand -hex 32)
TF_SYSTEM_PASSWORD=$(openssl rand -hex 32)
TF_PUBLIC_URL=http://localhost:3000
GH_CLIENT_ID=$GH_CLIENT_ID
GH_CLIENT_SECRET=$GH_CLIENT_SECRET
TF_SESSION_ENCRYPTION_KEY=$(openssl rand -hex 32)
TF_COOKIE_SECRET=$(openssl rand -hex 32)
TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
TF_BLOB_ACCESS_KEY=$(openssl rand -hex 32)
TF_BLOB_SECRET_KEY=$(openssl rand -hex 32)
GOTRUE_SAML_PRIVATE_KEY=$GOTRUE_SAML_PRIVATE_KEY
TF_LICENSE=$TF_LICENSE
TF_WS_SESSION_REVALIDATE_SECONDS=2
EOF
umask 022

# 5. The frontend and a binary that trusts this run's licence key. The key is
# an ldflags override on this build alone; release builds keep the source
# default.
log "building the frontend and the binary"
if [ "${SSO_E2E_SKIP_FRONTEND:-}" = 1 ] && [ -f "$ROOT/frontend/dist/index.html" ]; then
  echo "SSO_E2E_SKIP_FRONTEND=1: reusing frontend/dist"
else
  (cd "$ROOT/frontend" && pnpm install --frozen-lockfile && pnpm run build)
fi
go build -ldflags "-X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=$(cat "$STATE/license-pub.b64")" -o "$BIN" .

export MULTI_DEV_ENV_FILE="$STATE/env"
export MULTI_DEV_COMPOSE_OVERLAY="$SUITE/compose.e2e.yml"
export MULTI_DEV_BIN="$BIN"

# 6. The stack.
log "bringing up postgres, gotrue and seaweedfs"
"$DEV" up

# 7. The control half.
log "starting triagefactory (control)"
"$DEV" run control >"$STATE/tf.log" 2>&1 &
TF_PID=$!
ready=0
for _ in $(seq 1 120); do
  if curl -fsS -o /dev/null http://localhost:3000/readyz 2>/dev/null; then
    ready=1
    break
  fi
  kill -0 "$TF_PID" 2>/dev/null || break
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "triagefactory did not become ready — tail of $STATE/tf.log:" >&2
  tail -n 60 "$STATE/tf.log" >&2
  exit 1
fi

# GoTrue is each test IdP's one service provider. Its metadata exists only
# once GoTrue is up, so it is registered here rather than at IdP start.
log "registering GoTrue with both identity providers"
curl -fsS -o "$STATE/sp-metadata.xml" http://localhost:3000/auth/v1/sso/saml/metadata
for idp in idp-globex.test:8443 idp-mallory.test:8444; do
  fakes_curl -X PUT --data-binary @"$STATE/sp-metadata.xml" "https://$idp/services/gotrue"
done

# 8. The deployment operator, against the database `run` uses.
log "adding the deployment operator"
(
  set -a
  # shellcheck disable=SC1091
  source "$STATE/env"
  set +a
  TF_MODE=multi TF_ROLE=control \
    TF_DATABASE_URL="postgres://supabase_admin:${POSTGRES_PASSWORD}@localhost:5432/postgres" \
    "$BIN" operator add ops@operator.test
)

# 9. The suite.
log "running playwright"
cd "$SUITE"
pnpm install --frozen-lockfile
set +e
SSO_E2E_STATE="$STATE" pnpm exec playwright test
status=$?
set -e
exit "$status"
