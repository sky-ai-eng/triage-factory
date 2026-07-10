#!/bin/sh
# Triage Factory container entrypoint.
#
# Behavior summary:
#   1. Run goose-managed forward migrations, retrying on connection
#      failure so a not-yet-ready Postgres doesn't hard-fail the
#      container before compose/Fly finishes wiring DNS. Idempotent
#      and safe to re-run on every restart.
#   2. Privilege separation (multi mode only): spawn the cap-broker,
#      then exec into the orchestrator with its capabilities dropped.
#   3. exec the binary so tini/the container sees its signals
#      directly (no shell intermediary swallowing SIGTERM).
#
# Local-mode (the default) hits step 1 against the local SQLite file
# — no network races, succeeds immediately. So a plain `docker run`
# boots a working single-tenant TF without any env wiring at all.
# Local mode also never sandboxes (agentproc.WillSandbox is
# multi-mode-only) and has no broker to protect anything from — a
# single trusted operator already holds whatever the process holds —
# so step 2 is gated on TF_MODE=multi: it's skipped outright there,
# leaving local mode's default `docker run` exactly as unaffected as
# it was before this split existed, including uid and $HOME.
#
# Note on GoTrue keys: the entrypoint deliberately does NOT generate
# the GoTrue RS256 keypair. GoTrue runs in a separate container and
# reads GOTRUE_JWT_KEYS / GOTRUE_JWT_SECRET from its own env; a key
# generated inside THIS container can't reach it. Operators provision
# the keypair once on the host before `docker compose up`:
#
#   triagefactory jwk-init --write-env .env
#
# That writes the values into the compose .env which both services
# interpolate from. Same model for Fly: pass the generated values to
# the GoTrue deployment's secrets, not this image's environment.

set -eu

TF_HOME="${TF_HOME:-/root/.triagefactory}"

mkdir -p "$TF_HOME"

# Note: we deliberately do NOT source any .env file inside the
# container. `. "$ENV_FILE"` executes arbitrary shell, so a writable
# .env (via a separate vulnerability or a sloppy bind-mount) becomes
# a code-exec-on-restart path. Both supported deploy modes inject
# env vars directly into the process: docker-compose via the
# `environment:` block, Fly via `flyctl secrets set`. The TF binary
# reads everything from os.Getenv, so there's nothing for the shell
# to source.

# --- 1. Migrations (with bounded retry) ------------------------------------
#
# `triagefactory migrate up` opens the DB (Ping included) before
# invoking goose, so a connection failure surfaces here as a non-zero
# exit. Retrying the whole command — instead of probing connectivity
# separately — handles a few things at once:
#
#   - First boot: pg is reachable but goose tables don't exist yet.
#     A separate "wait" probe based on `migrate status` would
#     mis-classify this as "not ready" and burn the whole timeout.
#   - Restart with schema at head: migrate is a fast no-op.
#   - Truly unreachable DB: we surface the real error from migrate
#     after the retry budget is exhausted, so compose/Fly logs
#     contain the diagnostic instead of a generic "wait timed out".
#
# Goose's forward migrations are idempotent — re-running once pg is
# up is safe.

attempts=30
sleep_s=1
attempt=0
while :; do
    attempt=$((attempt + 1))
    if triagefactory migrate up; then
        break
    fi
    if [ "$attempt" -ge "$attempts" ]; then
        echo "migrate up failed after ${attempts} attempts; giving up." >&2
        exit 1
    fi
    echo "migrate up failed (attempt ${attempt}/${attempts}); retrying in ${sleep_s}s..." >&2
    sleep "$sleep_s"
done

# --- 2. Privilege separation: exec-time capability drop --------------------
#
# In multi mode on Linux — this container's sandbox is Linux-only and this
# whole mechanism exists to protect it — privilege separation is the only
# sandbox launch path. This entrypoint:
#
#   1. Spawns the cap-broker in the background. It stays root, holding
#      whatever this container was granted (SYS_ADMIN + NET_ADMIN on
#      top of Docker's own baseline set — see docker-compose.yml's
#      cap_add comment). It never touches credentials or hostile input.
#   2. Waits (bounded) for the broker's control socket to come up.
#   3. execs — REPLACING this shell, not forking a child — into the
#      orchestrator via setpriv, which atomically clears the capability
#      bounding set, drops inheritable capabilities, sets no_new_privs,
#      and switches to a different, unprivileged uid.
#
# This exec boundary is the actual drop, and it has to happen here, in
# a small single-threaded C program, rather than inside the Go
# orchestrator itself: Linux capabilities are per-thread, and the Go
# runtime spawns threads (e.g. sysmon) before main() ever runs — a
# capset()/prctl() call from Go code would only affect the one calling
# thread and silently leave every other thread still privileged, a
# control that looks applied but is not. setpriv drops privileges
# *before* execve()-ing the orchestrator binary, so every thread the Go
# runtime subsequently spawns inherits the already-dropped state from
# the very first instruction.
#
# The broker (root) and the orchestrator (TF_ORCHESTRATOR_UID) end up
# as different uids, so the orchestrator — holding zero capabilities,
# including no CAP_SYS_PTRACE, and a bounding set that can never regain
# any — cannot ptrace the broker even if it wanted to. Both stay in this
# container's single process group, so tini's SIGTERM fan-out (`-g` in
# the ENTRYPOINT below) reaches both for shutdown without the
# orchestrator needing to track a process it no longer has the
# privilege to signal-manage anyway.
# multi_mode mirrors internal/runmode.ModeFromEnv's own parsing exactly
# (case-insensitive match against "multi"; empty/unset means local; NOT
# whitespace-tolerant — runmode deliberately surfaces a stray-space typo
# rather than silently accepting it, so this doesn't trim either) — the
# same condition agentproc.WillSandbox() uses to decide whether this
# host sandboxes runs at all. Privsep only protects the sandbox's
# capabilities, so it has nothing to do in local mode: gating on this is
# what keeps the Dockerfile's own default (`TF_MODE=local`, "a docker run
# without any env vars boots into a working single-tenant binary") working
# exactly as before this split existed — no uid switch, no $HOME change,
# full privilege, matching every other single-operator local-mode
# deployment.
multi_mode() {
    [ "$(printf '%s' "${TF_MODE:-local}" | tr '[:upper:]' '[:lower:]')" = "multi" ]
}

if [ "$(uname -s)" = "Linux" ] && multi_mode; then
    TF_ORCHESTRATOR_UID="${TF_ORCHESTRATOR_UID:-10001}"
    TF_ORCHESTRATOR_GID="${TF_ORCHESTRATOR_GID:-10001}"
    TF_ORCHESTRATOR_HOME="${TF_ORCHESTRATOR_HOME:-/home/tf-orchestrator}"
    TF_CAPBROKER_SOCKET="${TF_CAPBROKER_SOCKET:-/run/tf/cap-broker.sock}"
    # The sandbox group (internal/sandbox.WorktreeGID). Carried through
    # the drop as the orchestrator's ONE supplementary group so it can
    # chgrp per-run agenthost sockets to the sandbox identity without
    # CAP_CHOWN — an owner-legal group grant; see grantSocketToSandbox in
    # cmd/exec/agenthost/socket_linux.go and the tf-sandbox group in
    # docker/Dockerfile.
    TF_SANDBOX_GID="${TF_SANDBOX_GID:-10000}"

    # Own the persistent state mount points so the (now non-root)
    # orchestrator can read/write its own data. Top-level only, not
    # recursive: cheap and idempotent on every boot, since content
    # created after this point is written directly by the orchestrator's
    # own uid. A volume populated by an older, root-running orchestrator
    # needs a one-time `chown -R` before its first boot under the new
    # default — see docs/self-host-setup.md.
    [ -d /data ] && chown "$TF_ORCHESTRATOR_UID:$TF_ORCHESTRATOR_GID" /data
    [ -d /opt/triagefactory/sandbox ] && chown "$TF_ORCHESTRATOR_UID:$TF_ORCHESTRATOR_GID" /opt/triagefactory/sandbox

    triagefactory cap-broker --socket "$TF_CAPBROKER_SOCKET" --orchestrator-uid "$TF_ORCHESTRATOR_UID" &
    BROKER_PID=$!

    # Bounded wait for the broker's control socket, mirroring the 10s
    # budget cmd/capbroker/orchestrator_linux.go's own readyTimeout uses
    # for the dev/bare-metal spawn-and-wait fallback path.
    attempts=10
    attempt=0
    while [ ! -S "$TF_CAPBROKER_SOCKET" ]; do
        if ! kill -0 "$BROKER_PID" 2>/dev/null; then
            echo "entrypoint: cap-broker exited before creating its socket" >&2
            exit 1
        fi
        attempt=$((attempt + 1))
        if [ "$attempt" -ge "$attempts" ]; then
            echo "entrypoint: cap-broker did not create its socket within ${attempts}s" >&2
            kill "$BROKER_PID" 2>/dev/null || true
            exit 1
        fi
        sleep 1
    done

    # setpriv switches uid/gid/capabilities but never touches environment
    # variables, so HOME (inherited from this shell's own environment —
    # /root, the container runtime's default for the root user) has to be
    # overridden explicitly here. Without this, os.UserHomeDir() in the
    # orchestrator (which Go resolves purely from $HOME on Linux, with no
    # /etc/passwd fallback) would still resolve to /root — a directory
    # the now-unprivileged orchestrator uid cannot read or write — for
    # every path internal/paths.go documents as "must follow the real
    # HOME even in multi mode" (Claude Code SDK session state: curator
    # sessions, skills import, project-bundle export/import).
    export HOME="$TF_ORCHESTRATOR_HOME"

    # --groups (not --clear-groups): sets the supplementary groups to
    # EXACTLY the sandbox group — still shedding root's group set like
    # --clear-groups did, while keeping the one membership the
    # orchestrator's owner-legal socket chgrp depends on.
    exec setpriv \
        --reuid="$TF_ORCHESTRATOR_UID" --regid="$TF_ORCHESTRATOR_GID" --groups="$TF_SANDBOX_GID" \
        --inh-caps=-all --bounding-set=-all --no-new-privs \
        -- triagefactory "$@"
fi

# --- 3. exec the binary -----------------------------------------------------
#
# The local-mode (default) / non-Linux path: exec replaces the shell with
# the Go process directly (still fully privileged, uid and $HOME untouched)
# so tini's signal forwarding lands directly on triagefactory. Without the
# exec, a SIGTERM from compose would hit this script and have to be relayed
# manually — losing the chance for graceful shutdown.

exec triagefactory "$@"
