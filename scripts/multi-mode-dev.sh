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
#   go build -o triagefactory .              # build the binary first
#   ./scripts/multi-mode-dev.sh up           # start deps, migrate schema (idempotent)
#   ./scripts/multi-mode-dev.sh run          # run the CONTROL half (API on :3000)
#   ./scripts/multi-mode-dev.sh run executor # run the EXECUTOR half (separate terminal)
#   ./scripts/multi-mode-dev.sh down         # stop deps, remove volumes
#
# Multi mode is always the control+executor split — the binary refuses to
# boot multi as a single fused process — so `run` takes the role to host.
# Most dev sessions only need `run` (control): the full API/WS/brain against
# real Postgres. Add `run executor` in a second terminal only when the work
# needs dispatch/sandboxing, which additionally requires a runsc-capable
# Linux host with the sandbox caps (the same bar as production executors).
# Each role gets its own TF_STATE_ROOT under .multi-dev-state/ so the two
# host processes never collide on the instance-identity flock.
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

# dc() reads DC_ENV_FILE rather than ENV_FILE directly so cmd_down's
# no-env-file branch can point it at a throwaway file (via a `local`
# override, restored once that function returns) instead of duplicating
# the docker compose invocation.
DC_ENV_FILE="$ENV_FILE"
dc() { docker compose -p "$PROJECT" --env-file "$DC_ENV_FILE" -f docker-compose.yml -f docker/compose.hostbind.yml "$@"; }

need_bin() {
  [ -x "$BIN" ] || { echo "no ./triagefactory binary — build one first: go build -o triagefactory ." >&2; exit 1; }
}

# `docker compose up --wait` treats a one-shot sidecar that exits 0 (by
# design — postgres-postinit, seaweedfs-postinit) as a failure, so it can't
# be used across this service set. Poll healthchecks for the long-running
# services and exit codes for the one-shots instead, mirroring
# scripts/compose-smoke.sh. A one-shot reaching Status=exited isn't enough
# on its own — ExitCode must be 0 too, or a failed role-password ALTER or
# bucket create (Status=exited, ExitCode!=0) would read as "ready" and the
# script would carry on against a partially-initialized stack.
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
      if [ -z "$cid" ]; then
        ready=0
        continue
      fi
      case "$(docker inspect -f '{{.State.Status}}' "$cid" 2>/dev/null)" in
        exited)
          code=$(docker inspect -f '{{.State.ExitCode}}' "$cid" 2>/dev/null)
          if [ "$code" != "0" ]; then
            echo "$svc exited $code (expected 0) — recent logs:" >&2
            dc logs --no-color --tail 50 "$svc" >&2
            return 1
          fi
          ;;
        *) ready=0 ;;
      esac
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
TF_SYSTEM_PASSWORD=$(openssl rand -hex 32)
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
  # Role is required in multi even for CLI subcommands (multi+all/unset
  # refuses to boot); control is the migrating role.
  TF_MODE=multi TF_ROLE=control TF_DATABASE_URL="postgres://supabase_admin:${POSTGRES_PASSWORD}@localhost:5432/postgres" \
    "$BIN" migrate up

  echo "Up. Run './scripts/multi-mode-dev.sh run' for the control half (and"
  echo "'./scripts/multi-mode-dev.sh run executor' in another terminal when you"
  echo "need dispatch), or 'down' to tear down."
}

cmd_run() {
  need_bin
  [ -f "$ENV_FILE" ] || { echo "no $ENV_FILE — run 'up' first" >&2; exit 1; }
  # First arg selects the role ONLY when it's a bare role token; a flag (or
  # nothing) means "control" and everything is forwarded to the binary, so
  # `run --log-level=debug` and `run executor --log-level=debug` both work.
  local role=control
  case "${1:-}" in
    control|executor) role=$1; shift ;;
    ""|-*) ;;
    *) echo "usage: $0 run [control|executor] [flags forwarded to triagefactory]" >&2; exit 1 ;;
  esac
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
  export TF_MODE=multi
  export TF_ROLE="$role"
  # Per-role state root: two host processes against one state root would
  # fail fast on the instance-identity flock, by design.
  export TF_STATE_ROOT="$ROOT/.multi-dev-state/$role"
  mkdir -p "$TF_STATE_ROOT"
  export TF_DATABASE_URL="postgres://supabase_admin:${POSTGRES_PASSWORD}@localhost:5432/postgres"
  export TF_GOTRUE_URL="http://localhost:9999"
  export TF_GOTRUE_JWKS_URL="http://localhost:9999/.well-known/jwks.json"
  export TF_GOTRUE_ISSUER="${PUBLIC_URL}/auth/v1"
  export TF_BLOB_ENDPOINT="http://localhost:8333"
  export TF_BLOB_BUCKET="${TF_BLOB_BUCKET:-tf-workspaces}"
  export TF_BLOB_REGION="${TF_BLOB_REGION:-us-east-1}"
  if [ "$role" = "executor" ]; then
    # No user HTTP on an executor — the localhost healthz is its only
    # listener. Sandboxing needs runsc + the sandbox caps on THIS host;
    # without them the boot fails loudly at cap-broker start (correct: an
    # executor that can't sandbox must not run).
    export TF_HEALTHZ_PORT="${TF_HEALTHZ_PORT:-3001}"
    exec "$BIN" --no-browser "$@"
  fi
  exec "$BIN" --port 3000 --no-browser "$@"
}

cmd_down() {
  if [ -f "$ENV_FILE" ]; then
    dc down -v --remove-orphans
    return
  fi
  # docker-compose.yml requires several ${VAR:?} vars just to PARSE
  # (compose loads/validates the full config regardless of subcommand), so
  # `--env-file $ENV_FILE` fails outright if that file is missing — e.g.
  # `down` before any `up`, or after deleting .env.multi-dev to reset
  # secrets. Synthesize a throwaway file with placeholder values (down
  # only needs the config to parse; it doesn't need real credentials) so
  # teardown still works. Never written as .env.multi-dev — a later `up`
  # still generates fresh secrets.
  echo "no $ENV_FILE — tearing down with placeholder values (config-parse only)"
  local tmp_env
  tmp_env=$(mktemp)
  # Double-quoted so $tmp_env expands NOW, embedding the literal path into
  # the trap command — a single-quoted trap would defer expansion until
  # the trap fires, by which point this function's `local` has gone out
  # of scope and $tmp_env is unbound (set -u then kills the trap itself).
  trap "rm -f $tmp_env" EXIT
  cat > "$tmp_env" <<EOF
POSTGRES_PASSWORD=unused
SUPABASE_AUTH_ADMIN_PASSWORD=unused
TF_AUTHENTICATOR_PASSWORD=unused
TF_SYSTEM_PASSWORD=unused
TF_PUBLIC_URL=$PUBLIC_URL
GH_CLIENT_ID=unused
GH_CLIENT_SECRET=unused
GOTRUE_JWT_KEYS=unused
GOTRUE_JWT_SECRET=unused
TF_SESSION_ENCRYPTION_KEY=unused
TF_COOKIE_SECRET=unused
TF_SECRET_ENCRYPTION_KEY=unused
TF_BLOB_ACCESS_KEY=unused
TF_BLOB_SECRET_KEY=unused
EOF
  local DC_ENV_FILE="$tmp_env"
  dc down -v --remove-orphans
}

case "${1:-}" in
  up) cmd_up ;;
  run) shift; cmd_run "$@" ;;
  down) cmd_down ;;
  *) echo "usage: $0 {up|run [control|executor]|down}" >&2; exit 1 ;;
esac
