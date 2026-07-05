#!/usr/bin/env bash
# On-demand multi-mode dev harness. NOT wired into scripts/session-start.sh —
# multi mode is heavier (Postgres + GoTrue + SeaweedFS) and most sessions
# never touch it, so this only runs when explicitly invoked.
#
# Brings up Postgres + GoTrue + SeaweedFS from the real docker-compose.yml
# (the same file self-host uses) but deliberately skips the triagefactory
# service — building its image from docker/Dockerfile requires apk access
# that isn't guaranteed in every environment (sandboxes included). Instead
# this runs the already-built host binary directly in TF_MODE=multi against
# the containers, which needs no image build at all.
#
# Usage:
#   go build -o triagefactory .         # build the binary first
#   ./scripts/multi-mode-dev.sh up      # start deps, migrate schema (idempotent)
#   ./scripts/multi-mode-dev.sh run     # run ./triagefactory in TF_MODE=multi
#   ./scripts/multi-mode-dev.sh down    # stop deps, remove volumes
#
# State: secrets + the GoTrue signing keypair persist in .env.multi-dev
# (gitignored) across runs, so `up` after a `down` reuses the same identity.
# Delete that file to start over with fresh secrets.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

PROJECT=tf-multi-dev
ENV_FILE="$ROOT/.env.multi-dev"
PUBLIC_URL=http://localhost:3000
BIN="$ROOT/triagefactory"
DEPS=(postgres postgres-postinit gotrue seaweedfs seaweedfs-postinit)

dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f docker-compose.yml -f docker/compose.hostbind.yml "$@"; }

need_bin() {
  [ -x "$BIN" ] || { echo "no ./triagefactory binary — build one first: go build -o triagefactory ." >&2; exit 1; }
}

# `docker compose up --wait` treats a one-shot sidecar that exits 0 (by
# design — postgres-postinit, seaweedfs-postinit) as a failure, so it can't
# be used across this service set. Poll healthchecks for the long-running
# services and exit codes for the one-shots instead, mirroring
# scripts/compose-smoke.sh.
wait_healthy() {
  local timeout=180 elapsed=0
  while :; do
    local ready=1
    for svc in postgres gotrue seaweedfs; do
      cid=$(dc ps -aq "$svc" | head -1)
      [ -n "$cid" ] && [ "$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null)" = "healthy" ] || ready=0
    done
    for svc in postgres-postinit seaweedfs-postinit; do
      cid=$(dc ps -aq "$svc" | head -1)
      [ -n "$cid" ] && [ "$(docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null)" = "exited" ] || ready=0
    done
    [ "$ready" = 1 ] && return 0
    [ "$elapsed" -ge "$timeout" ] && { echo "services not ready after ${timeout}s" >&2; dc ps; return 1; }
    sleep 2
    elapsed=$((elapsed + 2))
  done
}

cmd_up() {
  need_bin

  if [ ! -f "$ENV_FILE" ]; then
    echo "Generating $ENV_FILE (throwaway local secrets, gitignored)..."
    cat > "$ENV_FILE" <<EOF
POSTGRES_PASSWORD=$(openssl rand -hex 32)
SUPABASE_AUTH_ADMIN_PASSWORD=$(openssl rand -hex 32)
TF_AUTHENTICATOR_PASSWORD=$(openssl rand -hex 32)
TF_PUBLIC_URL=$PUBLIC_URL
GH_CLIENT_ID=multi-dev-no-real-oauth-app
GH_CLIENT_SECRET=$(openssl rand -hex 16)
TF_SESSION_ENCRYPTION_KEY=$(openssl rand -hex 32)
TF_COOKIE_SECRET=$(openssl rand -hex 32)
TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
TF_BLOB_ACCESS_KEY=$(openssl rand -hex 32)
TF_BLOB_SECRET_KEY=$(openssl rand -hex 32)
EOF
  fi
  # jwk-init upserts in place and reuses an existing GOTRUE_JWT_KEYS rather
  # than rotating it, so this is safe to re-run on every `up`.
  TF_PUBLIC_URL="$PUBLIC_URL" "$BIN" jwk-init --write-env "$ENV_FILE"

  echo "Starting postgres, gotrue, seaweedfs (skipping the triagefactory image build)..."
  dc up -d "${DEPS[@]}"
  wait_healthy

  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
  echo "Running migrations..."
  TF_MODE=multi TF_DATABASE_URL="postgres://supabase_admin:${POSTGRES_PASSWORD}@localhost:5432/postgres" \
    "$BIN" migrate up

  echo "Up. Run './scripts/multi-mode-dev.sh run' to start the server, or 'down' to tear down."
}

cmd_run() {
  need_bin
  [ -f "$ENV_FILE" ] || { echo "no $ENV_FILE — run 'up' first" >&2; exit 1; }
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
  export TF_MODE=multi
  export TF_DATABASE_URL="postgres://supabase_admin:${POSTGRES_PASSWORD}@localhost:5432/postgres"
  export TF_GOTRUE_URL="http://localhost:9999"
  export TF_GOTRUE_JWKS_URL="http://localhost:9999/.well-known/jwks.json"
  export TF_GOTRUE_ISSUER="${PUBLIC_URL}/auth/v1"
  export TF_BLOB_ENDPOINT="http://localhost:8333"
  export TF_BLOB_BUCKET="${TF_BLOB_BUCKET:-tf-workspaces}"
  export TF_BLOB_REGION="${TF_BLOB_REGION:-us-east-1}"
  exec "$BIN" --port 3000 --no-browser "$@"
}

cmd_down() {
  dc down -v --remove-orphans
}

case "${1:-}" in
  up) cmd_up ;;
  run) shift; cmd_run "$@" ;;
  down) cmd_down ;;
  *) echo "usage: $0 {up|run|down}" >&2; exit 1 ;;
esac
