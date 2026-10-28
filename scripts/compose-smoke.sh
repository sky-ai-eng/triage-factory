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
# must be REJECTED, not served allow-all, and each key pair reaches only its own
# bucket), the bucket sidecar and the versioning it sets, postgres role
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
WORKSPACE_BUCKET=tf-workspaces  # matches docker-compose.yml's TF_WORKSPACE_BUCKET default
KNOWLEDGE_BUCKET=tf-knowledge    # matches docker-compose.yml's TF_KNOWLEDGE_BUCKET default
PUBLIC_URL=http://localhost:3000
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-300}

# All compose commands go through the throwaway project + env file so a parallel
# real stack and the developer's .env are never touched. compose.dev.yml builds
# the TF image from the working tree in place of the release image the base
# file pins, so this tests the current code.
dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f docker-compose.yml -f compose.dev.yml "$@"; }

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
# The object-store pairs are kept in variables too: the scoping checks below
# sign requests as each of them.
WS_AK=$(openssl rand -hex 32) WS_SK=$(openssl rand -hex 32)
KW_AK=$(openssl rand -hex 32) KW_SK=$(openssl rand -hex 32)
KR_AK=$(openssl rand -hex 32) KR_SK=$(openssl rand -hex 32)
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
TF_WORKSPACE_ACCESS_KEY=$WS_AK
TF_WORKSPACE_SECRET_KEY=$WS_SK
TF_KNOWLEDGE_ACCESS_KEY=$KW_AK
TF_KNOWLEDGE_SECRET_KEY=$KW_SK
TF_KNOWLEDGE_READER_ACCESS_KEY=$KR_AK
TF_KNOWLEDGE_READER_SECRET_KEY=$KR_SK
SEAWEEDFS_ADMIN_ACCESS_KEY=$(openssl rand -hex 32)
SEAWEEDFS_ADMIN_SECRET_KEY=$(openssl rand -hex 32)
EOF

# Mint the GoTrue RS256 keypair + service-role token into the same file (the
# documented operator step). TF_PUBLIC_URL is exported so the token's iss claim
# matches; the file has no GOTRUE_JWT_KEYS yet, so this generates fresh material.
echo "Running jwk-init..."
TF_PUBLIC_URL="$PUBLIC_URL" "$BIN" jwk-init --write-env "$ENV_FILE"

# --- Bring the stack up ----------------------------------------------------
#
# Build from the working tree (compose.dev.yml, see dc above), not a published
# image. depends_on conditions make `up` wait for postgres/gotrue healthy and
# both postinit sidecars to complete successfully before starting control — so
# a sidecar that errors aborts `up` here with a non-zero exit.
# Clear any leftovers from a previously-crashed run so stale named volumes (an
# old bucket, an old migration set) from a reused project can't taint this run.
dc down -v --remove-orphans >/dev/null 2>&1 || true
echo "Building + starting the stack (this builds the TF image; first run is slow)..."
dc up -d --build

# control is the long pole: its depends_on transitively gates on every
# other service, and its healthcheck curls /api/health, which only answers once
# migrate-up succeeded and the server is serving. So waiting for it healthy is a
# full boot+migrate gate.
echo "Waiting for control to become healthy (timeout ${HEALTH_TIMEOUT}s)..."
# head -1: a service can map to >1 container id in a degraded project; inspect one.
cid=$(dc ps -aq control | head -1)
[ -n "$cid" ] || fail "control container was not created"
elapsed=0
while :; do
  status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || echo missing)
  case "$status" in
    healthy) break ;;
    exited|dead) fail "control $status before becoming healthy" ;;
  esac
  [ "$elapsed" -ge "$HEALTH_TIMEOUT" ] && fail "control not healthy after ${HEALTH_TIMEOUT}s (last status: $status)"
  sleep 3
  elapsed=$((elapsed + 3))
done
pass "control healthy (implies postgres + gotrue healthy, sidecars completed, migrate-up ran)"

# The executor is the second half of the default co-located split. It only
# starts after the control pod is healthy (its schema assert needs the
# migrated schema) and postgres-postinit-system completed (its admin pool
# authenticates as tf_system), so its healthy state is a full second-boot
# gate: role resolution accepted TF_ROLE=executor, the cap-broker came up,
# the dispatcher is heartbeating.
echo "Waiting for executor to become healthy (timeout ${HEALTH_TIMEOUT}s)..."
ecid=$(dc ps -aq executor | head -1)
[ -n "$ecid" ] || fail "executor container was not created"
elapsed=0
while :; do
  status=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$ecid" 2>/dev/null || echo missing)
  case "$status" in
    healthy) break ;;
    exited|dead) fail "executor $status before becoming healthy" ;;
  esac
  [ "$elapsed" -ge "$HEALTH_TIMEOUT" ] && fail "executor not healthy after ${HEALTH_TIMEOUT}s (last status: $status)"
  sleep 3
  elapsed=$((elapsed + 3))
done
pass "executor healthy (role accepted, schema assert passed, broker up, dispatcher heartbeating)"

# postgres-postinit-system (reconciles tf_system's password) deliberately
# starts only AFTER control is healthy — tf_system doesn't exist
# until control's entrypoint has migrated the schema, so this
# sidecar can't run any earlier (see its comment in docker-compose.yml).
# Unlike postgres-postinit/seaweedfs-postinit below, nothing gates on it
# finishing, so it isn't guaranteed done just because control is
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
#    postgres-postinit/seaweedfs-postinit (control depends_on them);
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

# 2. Both buckets exist and are reachable WITH credentials. head-bucket exits 0
#    only if the bucket is present and the request authenticated. Run through
#    the aws-cli sidecar on the compose network (seaweedfs publishes no host
#    port); it inherits the admin pair as AWS_* from the service env. Capture
#    output so a failure surfaces the real aws error (network vs auth vs missing
#    bucket), not just the generic message — head-bucket prints nothing on
#    success.
for bucket in "$WORKSPACE_BUCKET" "$KNOWLEDGE_BUCKET"; do
  if ! head_out=$(dc run --rm --no-deps --entrypoint aws seaweedfs-postinit \
      --endpoint-url http://seaweedfs:8333 s3api head-bucket --bucket "$bucket" 2>&1); then
    echo "--- head-bucket error ---" >&2
    echo "$head_out" >&2
    fail "bucket '$bucket' missing or not reachable with credentials"
  fi
  pass "bucket '$bucket' exists (authenticated head-bucket ok)"
done

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

# 4. Each key pair reaches only what its role needs. A key pair the s3.json
#    template scoped wrongly works fine for the role that holds it, so nothing
#    else here would notice: an executor holding a knowledge pair that can
#    write is exactly the hole the read-only pair closes. Each refusal is paired
#    with a request the same pair is allowed, so a refusal is the scoping and
#    not a key SeaweedFS failed to load.
s3_as() {
  local ak=$1 sk=$2
  shift 2
  dc run --rm --no-deps -T -e AWS_ACCESS_KEY_ID="$ak" -e AWS_SECRET_ACCESS_KEY="$sk" --entrypoint aws seaweedfs-postinit \
    --endpoint-url http://seaweedfs:8333 "$@"
}
s3_allowed() {
  local what=$1 out
  shift
  out=$(s3_as "$@" 2>&1) || { echo "$out" >&2; fail "$what was refused"; }
  pass "$what"
}
s3_denied() {
  local what=$1 out
  shift
  if out=$(s3_as "$@" 2>&1); then
    fail "$what SUCCEEDED — the key pair is not limited to what its role needs"
  fi
  case "$out" in
    *AccessDenied*|*403*|*Forbidden*) pass "$what refused (AccessDenied)" ;;
    *) echo "$out" >&2; fail "$what failed, but not with AccessDenied (scoping unclear)" ;;
  esac
}
s3_allowed "knowledge writer writes $KNOWLEDGE_BUCKET" "$KW_AK" "$KW_SK" s3api put-object --bucket "$KNOWLEDGE_BUCKET" --key smoke/writer
s3_allowed "knowledge reader reads $KNOWLEDGE_BUCKET" "$KR_AK" "$KR_SK" s3api head-object --bucket "$KNOWLEDGE_BUCKET" --key smoke/writer
s3_denied "knowledge reader's write to $KNOWLEDGE_BUCKET" "$KR_AK" "$KR_SK" s3api put-object --bucket "$KNOWLEDGE_BUCKET" --key smoke/reader
s3_allowed "workspace pair writes $WORKSPACE_BUCKET" "$WS_AK" "$WS_SK" s3api put-object --bucket "$WORKSPACE_BUCKET" --key smoke/workspace
s3_denied "knowledge writer's write to $WORKSPACE_BUCKET" "$KW_AK" "$KW_SK" s3api put-object --bucket "$WORKSPACE_BUCKET" --key smoke/writer

# 5. Versioning is on for the knowledge bucket and off for the workspace
#    bucket. The admin pair (the sidecar's own env) reads both; aws-cli prints
#    None for a bucket whose versioning was never set. Only stdout is captured:
#    compose reports the run container's lifecycle on stderr.
for pair in "$KNOWLEDGE_BUCKET:Enabled" "$WORKSPACE_BUCKET:None"; do
  bucket=${pair%%:*} want=${pair#*:}
  got=$(dc run --rm --no-deps -T --entrypoint aws seaweedfs-postinit \
    --endpoint-url http://seaweedfs:8333 s3api get-bucket-versioning --bucket "$bucket" --query Status --output text | tr -d '[:space:]') \
    || fail "get-bucket-versioning on '$bucket' failed"
  [ "$got" = "$want" ] || fail "versioning on '$bucket' is '$got', want '$want'"
done
pass "versioning enabled on '$KNOWLEDGE_BUCKET' only"

# 6. /api/health → 200 from the host's published port (the binary booted and is
#    serving). control publishes 3000:3000.
# No -f: it makes curl exit non-zero on an HTTP error and (version-dependently)
# garbles or suppresses the -w status code. Without it, curl exits 0 for any
# response so %{http_code} is the real status; || true keeps the connection-
# failure path graceful (curl prints 000 and exits non-zero there).
code=$(curl -sS -o /dev/null -w '%{http_code}' "$PUBLIC_URL/api/health" 2>/dev/null || true)
[ "$code" = "200" ] || fail "GET /api/health returned ${code:-<none>} (expected 200)"
pass "GET /api/health → 200"

# 7. Migrations actually applied. A healthy container already implies this (the
#    entrypoint exits non-zero if migrate-up never succeeds), but assert the
#    goose ledger has rows so the signal is explicit, not inferred.
gv=$(dc exec -T -e PGPASSWORD="$PG_PASSWORD" postgres \
  psql -U supabase_admin -d postgres -tAc "SELECT count(*) FROM goose_db_version" 2>/dev/null | tr -d '[:space:]' || echo "")
case "$gv" in
  ''|*[!0-9]*) fail "could not read goose_db_version (migrations may not have run): '$gv'" ;;
  0) fail "goose_db_version is empty — migrations did not apply" ;;
  *) pass "migrations applied (goose_db_version has $gv rows)" ;;
esac

# 8. Privilege-separation posture, per role. The failure class this catches
#    is invisible to the health probes: a container boots green while the
#    split is silently degraded (a broker on the capless control pod, an
#    orchestrator that kept its capabilities or a supplementary group, or a
#    /run/tf a run's sidecar could write in). Multi mode is always the
#    control+executor split, so BOTH postures are unconditionally expected:
#      control  — NO cap-broker (it never launches a sandbox and its service
#                 carries no caps), orchestrator capless + non-root.
#      executor — cap-broker present (the only sandbox launch path),
#                 orchestrator capless + non-root with no supplementary
#                 group, /run/tf orchestrator-owned at 0711.

# 8a. Control: capless, broker-free.
posture=$(dc exec -T control sh -c '
  broker=""; orch=""
  for c in /proc/[0-9]*/comm; do
    read -r name < "$c" 2>/dev/null || continue
    pid=${c#/proc/}; pid=${pid%%/*}
    case "$name" in
      tf-cap-broker)    broker=$pid ;;
      tf-orchestrator)  orch=$pid ;;
    esac
  done
  [ -z "$broker" ] || { echo "control pod runs a tf-cap-broker (pid $broker); control must never spawn one"; exit 1; }
  [ -n "$orch" ]   || { echo "no tf-orchestrator process found"; exit 1; }
  eff=$(awk "/^CapEff/{print \$2}" "/proc/$orch/status")
  bnd=$(awk "/^CapBnd/{print \$2}" "/proc/$orch/status")
  [ "$eff" = "0000000000000000" ] || { echo "control orchestrator CapEff=$eff, want all-zero"; exit 1; }
  [ "$bnd" = "0000000000000000" ] || { echo "control orchestrator CapBnd=$bnd, want all-zero (bounding set must be cleared)"; exit 1; }
  ouid=$(awk "/^Uid/{print \$2}" "/proc/$orch/status")
  [ "$ouid" != "0" ] || { echo "control orchestrator runs as root; the uid switch did not apply"; exit 1; }
  echo "no broker; orchestrator pid $orch (uid $ouid, CapEff/CapBnd zero)"
' 2>&1) || fail "control privsep posture: $posture"
pass "control privsep posture: $posture"

# 8b. Executor: broker-then-drop split.
posture=$(dc exec -T executor sh -c '
  broker=""; orch=""
  for c in /proc/[0-9]*/comm; do
    read -r name < "$c" 2>/dev/null || continue
    pid=${c#/proc/}; pid=${pid%%/*}
    case "$name" in
      tf-cap-broker)    broker=$pid ;;
      tf-orchestrator)  orch=$pid ;;
    esac
  done
  [ -n "$broker" ] || { echo "no tf-cap-broker process found (the only sandbox launch path)"; exit 1; }
  [ -n "$orch" ]   || { echo "no tf-orchestrator process found"; exit 1; }
  eff=$(awk "/^CapEff/{print \$2}" "/proc/$orch/status")
  bnd=$(awk "/^CapBnd/{print \$2}" "/proc/$orch/status")
  [ "$eff" = "0000000000000000" ] || { echo "executor orchestrator CapEff=$eff, want all-zero"; exit 1; }
  [ "$bnd" = "0000000000000000" ] || { echo "executor orchestrator CapBnd=$bnd, want all-zero (bounding set must be cleared)"; exit 1; }
  ouid=$(awk "/^Uid/{print \$2}" "/proc/$orch/status")
  [ "$ouid" != "0" ] || { echo "executor orchestrator runs as root; the uid switch did not apply"; exit 1; }
  ogroups=$(awk "/^Groups:/{sub(/^Groups:[[:space:]]*/, \"\"); print}" "/proc/$orch/status")
  [ -z "$ogroups" ] || { echo "executor orchestrator carries supplementary groups \"$ogroups\"; it must carry none (a jail gid would make it a member of that run)"; exit 1; }
  duid=$(stat -c %u /run/tf)
  [ "$duid" = "$ouid" ] || { echo "/run/tf owned by uid $duid, want the orchestrator uid $ouid (its per-launch stdio sockets are created there unprivileged)"; exit 1; }
  dmode=$(stat -c %a /run/tf)
  [ "$dmode" = "711" ] || { echo "/run/tf mode $dmode, want 711 (no group or other write: a sidecar creates only inside its own run directory)"; exit 1; }
  echo "broker pid $broker (root, caps); orchestrator pid $orch (uid $ouid, CapEff/CapBnd zero, no groups); /run/tf $duid 0711"
' 2>&1) || fail "executor privsep posture: $posture"
pass "executor privsep posture: $posture"

# 8c. Executor: every host tool the cap-broker runs by name is on its PATH.
#     A missing one boots green and fails only when the broker first runs it
#     (prlimit, for instance, on the first workspace capture).
tools=$(dc exec -T executor sh -c '
  missing=""
  for t in runsc prlimit iptables ip; do
    command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
  done
  [ -z "$missing" ] || { echo "missing from the executor image:$missing"; exit 1; }
  echo "runsc, prlimit, iptables, ip"
' 2>&1) || fail "executor host tools: $tools"
pass "executor host tools on PATH: $tools"

# 9. The observability stack. Each of these fails silently — no crash, no 503,
#    just an empty UI weeks later when someone finally looks.

# 9a. TF resolved the endpoint and installed a tracer provider: the compose
#     default reached the process env AND the binary accepted it. A dropped env
#     line leaves every other check in this script passing.
#
#     Read the log into a variable and match in the shell rather than piping
#     into `grep -q`. Under `set -o pipefail` that pipeline is a race: grep
#     exits on its first match and closes the pipe, `docker compose logs` dies
#     of SIGPIPE (141), and pipefail hands 141 to the `if` — so the check fails
#     *because* it found the line, whenever the container logged enough after
#     it that compose was still writing. The line it looks for is emitted at
#     boot and the pod logs steadily afterwards, so this got likelier the
#     longer the checks above took.
tf_logs=$(dc logs control 2>/dev/null || true)
case "$tf_logs" in
  *"tracing enabled"*) ;;
  *) fail "control pod never logged 'tracing enabled' — TF_TRACES_ENDPOINT didn't reach it, or the exporter refused the value" ;;
esac
pass "control pod installed a tracer provider (logged 'tracing enabled')"

# 9b. Tempo accepts OTLP over HTTP on the address that env var names, and hands
#     the trace back. Retrieval is by ID, not search: an ID lookup hits the
#     ingester immediately, while search waits for the trace to be cut (~10s)
#     and would make this flaky for no extra coverage. curl runs from the
#     grafana container — Tempo's image is distroless and has none. The
#     payload is built here and piped over stdin rather than embedded in the
#     remote shell's argv, so there is exactly one level of quoting to get
#     right. Second-precision timestamps: macOS `date` has no %N, and a span
#     stamped in 1970 stores fine and then never appears in any search.
trace_id=1234567890abcdef1234567890abcdef
now=$(date +%s)
printf '%s' "{\"resourceSpans\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"compose-smoke\"}}]},
\"scopeSpans\":[{\"spans\":[{\"traceId\":\"$trace_id\",\"spanId\":\"1234567890abcdef\",
\"name\":\"smoke.test\",\"kind\":1,
\"startTimeUnixNano\":\"$((now - 1))000000000\",\"endTimeUnixNano\":\"${now}000000000\",
\"attributes\":[{\"key\":\"conversation.id\",\"value\":{\"stringValue\":\"compose-smoke\"}}]}]}]}]}" \
  | dc exec -T grafana curl -sS -X POST http://tempo:4318/v1/traces \
      -H 'content-type: application/json' --data-binary @- >/dev/null \
  || fail "OTLP push to tempo:4318 failed"
#     Same shell-side matching as above, and for the same reason — a trace
#     body is large enough for the SIGPIPE race to be real, and here it would
#     burn all 15 attempts before reporting a span that had arrived.
stored=""
for _ in $(seq 1 15); do
  body=$(dc exec -T grafana curl -sS "http://tempo:3200/api/traces/$trace_id" 2>/dev/null || true)
  case "$body" in
    *compose-smoke*) stored=yes; break ;;
  esac
  sleep 2
done
[ -n "$stored" ] || fail "tempo accepted an OTLP span but never returned it from /api/traces/$trace_id"
pass "tempo ingests OTLP/HTTP on :4318 and serves the trace back"

# 9c. The metrics-generator answers a TraceQL metrics query — the query class
#     Grafana's Traces Drilldown is built on, and a different path through
#     Tempo from 9b's lookup-by-ID. It runs after 9b deliberately: the
#     generator only holds a tenant once spans have reached it, and until then
#     a missing local-blocks processor answers an empty result instead of the
#     error it owes you. With a tenant in place the two failures separate
#     cleanly — no processor is an explicit `localblocks processor not found`,
#     and an empty answer means the query ran and matched nothing.
metrics=""
for _ in $(seq 1 15); do
  body=$(dc exec -T grafana curl -sS -G http://tempo:3200/api/metrics/query_range \
    --data-urlencode 'q={ name = "smoke.test" } | count_over_time()' \
    --data-urlencode "start=$((now - 60))" --data-urlencode "end=$(date +%s)" \
    --data-urlencode 'step=60s' 2>/dev/null || true)
  case "$body" in
    *"localblocks processor not found"*)
      fail "tempo's metrics-generator has no local-blocks processor — every TraceQL metrics query and all of Grafana's Traces Drilldown fail (check metrics_generator in docker/observability/tempo.yaml)" ;;
  esac
  #     Match a sample that carries a count. Every label object in the
  #     response also has a `"value"` key, so the loose spelling would pass on
  #     an answer of nothing but empty buckets; a timestamp-then-value pair is
  #     a filled one. In-shell, like the case above and for the same reason —
  #     piping the body to grep -q races the reader's exit. A span lands in a
  #     bucket about ten seconds after the push, once it has gone idle.
  if [[ "$body" =~ \"timestampMs\":\"[0-9]+\",\"value\": ]]; then metrics=yes; break; fi
  sleep 2
done
[ -n "$metrics" ] || fail "tempo never counted the span 9b pushed in a TraceQL metrics query (metrics_generator.processors and traces_storage.path in docker/observability/tempo.yaml)"
pass "tempo's local-blocks processor answers TraceQL metrics (what Traces Drilldown queries)"

# 9d. Grafana's provisioning actually applied. Provisioning failures are
#     logged and skipped, not fatal, so a malformed data source or correlation
#     leaves a running Grafana that simply cannot navigate anything — and the
#     correlations are the entire point of the file.
prov=""
for _ in $(seq 1 30); do
  ds=$(dc exec -T grafana curl -sS http://localhost:3000/api/datasources 2>/dev/null || true)
  corr=$(dc exec -T grafana curl -sS http://localhost:3000/api/datasources/correlations 2>/dev/null || true)
  case "$ds" in *tf-tempo*) case "$ds" in *tf-prometheus*) prov="$corr" ;; esac ;; esac
  [ -n "$prov" ] && break
  sleep 2
done
[ -n "$prov" ] || fail "grafana did not provision both data sources (tf-tempo + tf-prometheus)"
for attr in conversation.id event.id task.id; do
  case "$prov" in
    *"$attr"*) ;;
    *) fail "grafana is missing the $attr span correlation (provisioning error — check its logs)" ;;
  esac
done
pass "grafana provisioned both data sources + the conversation/event/task correlations"

# 9e. The dashboards half of the same provisioning, and the same failure mode:
#     a malformed dashboard JSON is logged and skipped, leaving a Grafana that
#     starts fine and shows an empty home page. Three distinct regressions,
#     each invisible without its own assertion:
#
#       - the dashboard file didn't load at all (bad JSON, wrong mount path)
#       - it loaded, but GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH names a
#         different path than the mount, so :3030 opens on Grafana's stock home
#         page and every panel here goes unseen
#       - it loaded and is the home page, but its panels reference a data
#         source UID nothing provisions, so every panel errors
home=""
for _ in $(seq 1 30); do
  dash=$(dc exec -T grafana curl -sS http://localhost:3000/api/dashboards/uid/tf-overview 2>/dev/null || true)
  case "$dash" in
    *'"uid":"tf-overview"'*) home=$(dc exec -T grafana curl -sS http://localhost:3000/api/dashboards/home 2>/dev/null || true) ;;
  esac
  [ -n "$home" ] && break
  sleep 2
done
[ -n "$home" ] || fail "grafana did not provision the tf-overview dashboard (bad JSON or a mount-path mismatch — check its logs)"
case "$home" in
  *'"uid":"tf-overview"'*) ;;
  *) fail "grafana's home dashboard is not tf-overview — GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH does not resolve to the mounted file, so :3030 opens on the stock home page" ;;
esac
for uid in tf-tempo tf-prometheus; do
  case "$dash" in
    *"$uid"*) ;;
    *) fail "the tf-overview dashboard references no $uid data source — a UID typo would leave its panels erroring" ;;
  esac
done
pass "grafana provisioned tf-overview, serves it as the home dashboard, and its panels address both data sources"

# 9f. Prometheus scrapes both roles. It finds the pods by their compose
#     service names (dns_sd_configs in docker/observability/prometheus.yml), so
#     a service renamed in docker-compose.yml and not there resolves to nothing:
#     the job has no targets, every TF panel goes blank, and no alert fires.
#     The tf_service label is the service name each target was found through,
#     which is what the dashboards and any operator alert filter on. The
#     discovery refreshes every 30s and the stack has been up for minutes by
#     now, so this normally passes on the first read.
scraped=""
for _ in $(seq 1 30); do
  up=$(dc exec -T grafana curl -sS -G http://prometheus:9090/api/v1/query \
    --data-urlencode 'query=count by (tf_service) (up{job="triagefactory"} == 1)' 2>/dev/null || true)
  case "$up" in
    *'"tf_service":"control"'*) case "$up" in *'"tf_service":"executor"'*) scraped=yes ;; esac ;;
  esac
  [ -n "$scraped" ] && break
  sleep 3
done
[ -n "$scraped" ] || fail "prometheus is not scraping both TF roles as tf_service=control and tf_service=executor (last answer: ${up:-<none>}) — check that dns_sd_configs names in docker/observability/prometheus.yml match the service names in docker-compose.yml"
pass "prometheus scrapes both roles (tf_service=control, tf_service=executor)"

echo ""
echo "compose-smoke: ALL CHECKS PASSED ✓"
