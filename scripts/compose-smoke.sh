#!/usr/bin/env bash
# Triage Factory — multi-mode compose-stack smoke test.
#
# Brings the WHOLE self-host stack up against a throwaway generated .env and
# asserts the deployment contract that Go unit tests can't reach: the service
# entrypoints, healthchecks, one-shot sidecars, and cross-service wiring.
#
# Why this exists separately from the Go tests: `internal/storage` conformance
# exercises the S3 *client* against a `weed mini` (allow-all) testcontainer — it
# never touches the compose *deploy* surface. This script does: the seaweedfs
# entrypoint's s3.json templating + identity auth (an unauthenticated request
# must be REJECTED, not served allow-all), the bucket sidecar, postgres role
# reconciliation, GoTrue, and the TF binary's migrate-up + boot. TFAC-377's
# compose-level bugs (JSON-injection footgun, tmpfs read perms, auth-on vs
# allow-all) were caught only by bringing the stack up by hand; this automates
# that so the next person editing a wrapper has a regression net.
#
# Run locally (needs Docker + a host binary for jwk-init — build per README):
#   cd frontend && pnpm install && pnpm run build && cd ..
#   go build -o triagefactory .
#   ./scripts/compose-smoke.sh
# Point at an existing binary instead:
#   TRIAGEFACTORY_BIN=/path/to/triagefactory ./scripts/compose-smoke.sh
#
# CI runs this same script (.github/workflows/compose-smoke.yml). Everything is
# isolated under a dedicated compose project + throwaway env file and torn down
# (down -v) on exit, so it never touches a developer's real stack or .env.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

# Override-able so concurrent runs (or a coincidental real project of the same
# name) don't collide; the default is an obvious throwaway.
PROJECT="${TF_SMOKE_PROJECT:-tf-smoke}"
ENV_FILE=$(mktemp "${TMPDIR:-/tmp}/tf-smoke-env.XXXXXX")
BUCKET=tf-workspaces            # matches docker-compose.yml's TF_BLOB_BUCKET default
PUBLIC_URL=http://localhost:3000
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-300}

# All compose commands go through the throwaway project + env file so a parallel
# real stack and the developer's .env are never touched.
dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" "$@"; }

# Tear the stack down and remove the env file on ANY exit. On failure, dump
# compose logs first so CI (and a local run) has the diagnostic inline.
cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    echo ""
    echo "=== compose-smoke FAILED (exit $code) — recent compose logs ==="
    dc logs --no-color --tail 200 2>/dev/null || true
  fi
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$ENV_FILE" "${unauth_out:-}"
}
trap cleanup EXIT

fail() { echo "  ✗ $*" >&2; exit 1; }
pass() { echo "  ✓ $*"; }

# --- Preconditions ---------------------------------------------------------

command -v docker >/dev/null 2>&1 || fail "docker not found on PATH"
docker compose version >/dev/null 2>&1 || fail "'docker compose' (v2) not available"
command -v openssl >/dev/null 2>&1 || fail "openssl not found on PATH (needed to generate throwaway secrets)"
command -v curl >/dev/null 2>&1 || fail "curl not found on PATH (needed for the /api/health probe)"

# jwk-init must run from a built TF binary — GoTrue won't boot without valid
# RS256 signing material, and re-implementing the keygen here would drift from
# the real one. Resolve an explicit override, then the repo-root build, then PATH.
BIN=${TRIAGEFACTORY_BIN:-}
if [ -z "$BIN" ]; then
  if [ -x "$ROOT/triagefactory" ]; then
    BIN="$ROOT/triagefactory"
  elif command -v triagefactory >/dev/null 2>&1; then
    BIN=$(command -v triagefactory)
  fi
fi
[ -n "$BIN" ] && [ -x "$BIN" ] || fail "no triagefactory binary found — build one first (cd frontend && pnpm install && pnpm run build && cd .. && go build -o triagefactory .) or set TRIAGEFACTORY_BIN"

# --- Throwaway secrets -----------------------------------------------------
#
# Every required value is generated fresh; nothing real is involved. The
# GitHub OAuth creds are deliberately bogus — GoTrue only *registers* the
# provider at boot (it doesn't validate the creds until an actual OAuth flow,
# which the smoke test never triggers), so dummy values keep the stack healthy.

echo "Generating throwaway .env ($ENV_FILE)..."
PG_PASSWORD=$(openssl rand -hex 32)
cat > "$ENV_FILE" <<EOF
POSTGRES_PASSWORD=$PG_PASSWORD
SUPABASE_AUTH_ADMIN_PASSWORD=$(openssl rand -hex 32)
TF_AUTHENTICATOR_PASSWORD=$(openssl rand -hex 32)
TF_SYSTEM_PASSWORD=$(openssl rand -hex 32)
TF_PUBLIC_URL=$PUBLIC_URL
GH_CLIENT_ID=smoke-test-no-real-oauth-app
GH_CLIENT_SECRET=$(openssl rand -hex 16)
TF_SESSION_ENCRYPTION_KEY=$(openssl rand -hex 32)
TF_COOKIE_SECRET=$(openssl rand -hex 32)
TF_SECRET_ENCRYPTION_KEY=$(openssl rand -hex 32)
TF_BLOB_ACCESS_KEY=$(openssl rand -hex 32)
TF_BLOB_SECRET_KEY=$(openssl rand -hex 32)
EOF

# Mint the GoTrue RS256 keypair + service-role token into the same file (the
# documented operator step). TF_PUBLIC_URL is exported so the token's iss claim
# matches; the file has no GOTRUE_JWT_KEYS yet, so this generates fresh material.
echo "Running jwk-init..."
TF_PUBLIC_URL="$PUBLIC_URL" "$BIN" jwk-init --write-env "$ENV_FILE"

# --- Bring the stack up ----------------------------------------------------
#
# Build from the working tree (the triagefactory service uses build:, so this
# tests the current code, not a published image). depends_on conditions make
# `up` wait for postgres/gotrue healthy and both postinit sidecars to complete
# successfully before starting triagefactory — so a sidecar that errors aborts
# `up` here with a non-zero exit.
# Clear any leftovers from a previously-crashed run so stale named volumes (an
# old bucket, an old migration set) from a reused project can't taint this run.
dc down -v --remove-orphans >/dev/null 2>&1 || true
echo "Building + starting the stack (this builds the TF image; first run is slow)..."
dc up -d --build

# triagefactory is the long pole: its depends_on transitively gates on every
# other service, and its healthcheck curls /api/health, which only answers once
# migrate-up succeeded and the server is serving. So waiting for it healthy is a
# full boot+migrate gate.
echo "Waiting for triagefactory to become healthy (timeout ${HEALTH_TIMEOUT}s)..."
# head -1: a service can map to >1 container id in a degraded project; inspect one.
cid=$(dc ps -aq triagefactory | head -1)
[ -n "$cid" ] || fail "triagefactory container was not created"
elapsed=0
while :; do
  status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || echo missing)
  case "$status" in
    healthy) break ;;
    exited|dead) fail "triagefactory $status before becoming healthy" ;;
  esac
  [ "$elapsed" -ge "$HEALTH_TIMEOUT" ] && fail "triagefactory not healthy after ${HEALTH_TIMEOUT}s (last status: $status)"
  sleep 3
  elapsed=$((elapsed + 3))
done
pass "all services healthy (triagefactory up — implies postgres + gotrue healthy, sidecars completed, migrate-up ran)"

# postgres-postinit-system (reconciles tf_system's password) deliberately
# starts only AFTER triagefactory is healthy — tf_system doesn't exist
# until triagefactory's entrypoint has migrated the schema, so this
# sidecar can't run any earlier (see its comment in docker-compose.yml).
# Unlike postgres-postinit/seaweedfs-postinit below, nothing gates on it
# finishing, so it isn't guaranteed done just because triagefactory is
# healthy — wait for it explicitly before asserting its exit code.
echo "Waiting for postgres-postinit-system to complete..."
pscid=$(dc ps -aq postgres-postinit-system | head -1)
[ -n "$pscid" ] || fail "postgres-postinit-system container was not created"
elapsed=0
while :; do
  status=$(docker inspect -f '{{.State.Status}}' "$pscid" 2>/dev/null || echo missing)
  [ "$status" = "exited" ] && break
  [ "$elapsed" -ge 60 ] && fail "postgres-postinit-system not exited after 60s (last status: $status)"
  sleep 2
  elapsed=$((elapsed + 2))
done

# --- Assert the deployment contract ----------------------------------------

echo "Asserting the deployment contract..."

# 1. One-shot sidecars exited 0. `up` already gates dependents on this for
#    postgres-postinit/seaweedfs-postinit (triagefactory depends_on them);
#    postgres-postinit-system is gated by the explicit wait above instead,
#    since nothing depends on it. Assert all three explicitly so a
#    regression names the offending sidecar.
for svc in postgres-postinit seaweedfs-postinit postgres-postinit-system; do
  scid=$(dc ps -aq "$svc" | head -1)
  [ -n "$scid" ] || fail "$svc container was not created"
  ec=$(docker inspect -f '{{.State.ExitCode}}' "$scid")
  [ "$ec" = "0" ] || fail "$svc exited $ec (expected 0)"
done
pass "postgres-postinit + seaweedfs-postinit + postgres-postinit-system exited 0"

# 2. The workspace bucket exists and is reachable WITH credentials. head-bucket
#    exits 0 only if the bucket is present and the request authenticated. Run
#    through the aws-cli sidecar on the compose network (seaweedfs publishes no
#    host port); it inherits AWS_* from the service env. Capture output so a
#    failure surfaces the real aws error (network vs auth vs missing bucket),
#    not just the generic message — head-bucket prints nothing on success.
if ! head_out=$(dc run --rm --no-deps --entrypoint aws seaweedfs-postinit \
    --endpoint-url http://seaweedfs:8333 s3api head-bucket --bucket "$BUCKET" 2>&1); then
  echo "--- head-bucket error ---" >&2
  echo "$head_out" >&2
  fail "bucket '$BUCKET' missing or not reachable with credentials"
fi
pass "bucket '$BUCKET' exists (authenticated head-bucket ok)"

# 3. THE load-bearing assertion: an UNAUTHENTICATED request must be REJECTED.
#    SeaweedFS serves allow-all when no identity is configured; this proves the
#    templated s3.json identity actually turned auth ON. --no-sign-request sends
#    no Authorization header. Success here = allow-all regression = hard fail.
unauth_out=$(mktemp "${TMPDIR:-/tmp}/tf-smoke-unauth.XXXXXX")
if dc run --rm --no-deps -e AWS_ACCESS_KEY_ID="" -e AWS_SECRET_ACCESS_KEY="" --entrypoint aws seaweedfs-postinit \
    --endpoint-url http://seaweedfs:8333 --no-sign-request s3api list-buckets >"$unauth_out" 2>&1; then
  rm -f "$unauth_out"
  fail "unauthenticated S3 request SUCCEEDED — the bundled store is allow-all (identity auth not enforced)"
fi
# It failed as required; confirm it's an auth denial and not some unrelated error
# (network, DNS, malformed config) that would mask an actual allow-all elsewhere.
if grep -Eq '403|AccessDenied|Forbidden' "$unauth_out"; then
  pass "unauthenticated S3 request rejected (403 / AccessDenied)"
  rm -f "$unauth_out"
else
  echo "--- unexpected unauth error output ---" >&2
  cat "$unauth_out" >&2
  rm -f "$unauth_out"
  fail "unauthenticated S3 request failed, but not with 403/AccessDenied (auth state unclear)"
fi

# 4. /api/health → 200 from the host's published port (the binary booted and is
#    serving). triagefactory publishes 3000:3000.
# No -f: it makes curl exit non-zero on an HTTP error and (version-dependently)
# garbles or suppresses the -w status code. Without it, curl exits 0 for any
# response so %{http_code} is the real status; || true keeps the connection-
# failure path graceful (curl prints 000 and exits non-zero there).
code=$(curl -sS -o /dev/null -w '%{http_code}' "$PUBLIC_URL/api/health" 2>/dev/null || true)
[ "$code" = "200" ] || fail "GET /api/health returned ${code:-<none>} (expected 200)"
pass "GET /api/health → 200"

# 5. Migrations actually applied. A healthy container already implies this (the
#    entrypoint exits non-zero if migrate-up never succeeds), but assert the
#    goose ledger has rows so the signal is explicit, not inferred.
gv=$(dc exec -T -e PGPASSWORD="$PG_PASSWORD" postgres \
  psql -U supabase_admin -d postgres -tAc "SELECT count(*) FROM goose_db_version" 2>/dev/null | tr -d '[:space:]' || echo "")
case "$gv" in
  ''|*[!0-9]*) fail "could not read goose_db_version (migrations may not have run): '$gv'" ;;
  0) fail "goose_db_version is empty — migrations did not apply" ;;
  *) pass "migrations applied (goose_db_version has $gv rows)" ;;
esac

# 6. Privilege-separation posture. The failure class this catches is
#    invisible to the health probe: the container boots green while the
#    split is silently degraded (one process, or an orchestrator that kept
#    its capabilities, or a /run/tf the unprivileged orchestrator can't
#    create per-run agenthost sockets in — every delegated run would then
#    fail at its first sandbox setup, long after "healthy"). The compose
#    stack pins TF_MODE=multi, and the cap-broker is the only sandbox launch
#    path, so the split is unconditionally expected here.
posture=$(dc exec -T triagefactory sh -c '
  broker=""; orch=""
  for c in /proc/[0-9]*/comm; do
    read -r name < "$c" 2>/dev/null || continue
    pid=${c#/proc/}; pid=${pid%%/*}
    case "$name" in
      tf-cap-broker)    broker=$pid ;;
      tf-orchestrator)  orch=$pid ;;
    esac
  done
  [ -n "$broker" ] || { echo "no tf-cap-broker process found"; exit 1; }
  [ -n "$orch" ]   || { echo "no tf-orchestrator process found"; exit 1; }
  eff=$(awk "/^CapEff/{print \$2}" "/proc/$orch/status")
  bnd=$(awk "/^CapBnd/{print \$2}" "/proc/$orch/status")
  [ "$eff" = "0000000000000000" ] || { echo "orchestrator CapEff=$eff, want all-zero"; exit 1; }
  [ "$bnd" = "0000000000000000" ] || { echo "orchestrator CapBnd=$bnd, want all-zero (bounding set must be cleared)"; exit 1; }
  ouid=$(awk "/^Uid/{print \$2}" "/proc/$orch/status")
  [ "$ouid" != "0" ] || { echo "orchestrator runs as root; the uid switch did not apply"; exit 1; }
  duid=$(stat -c %u /run/tf)
  [ "$duid" = "$ouid" ] || { echo "/run/tf owned by uid $duid, want the orchestrator uid $ouid (agenthost sockets are created there unprivileged)"; exit 1; }
  echo "broker pid $broker (root, caps); orchestrator pid $orch (uid $ouid, CapEff/CapBnd zero); /run/tf owned by $ouid"
' 2>&1) || fail "privsep posture: $posture"
pass "privsep posture: $posture"

echo ""
echo "compose-smoke: ALL CHECKS PASSED ✓"
