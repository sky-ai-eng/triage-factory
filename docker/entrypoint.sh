#!/bin/sh
# Triage Factory container entrypoint.
#
# Behavior summary:
#   1. Run goose-managed forward migrations, retrying on connection
#      failure so a not-yet-ready Postgres doesn't hard-fail the
#      container before compose/Fly finishes wiring DNS. Idempotent
#      and safe to re-run on every restart.
#   2. Privilege separation (multi mode only): stage file-backed secrets
#      for the orchestrator uid, spawn the cap-broker on a sandbox-hosting
#      role (all, executor) — control skips it — then exec into the
#      orchestrator with its capabilities dropped, on every role.
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

# Exit code 3 (cmd/migrate.ExitSchemaAhead) is special: a TF_ROLE=executor
# process found the schema AHEAD of what its build understands. That never
# self-resolves by waiting — the fix is deploying a newer executor image
# first (drain-first-on-schema-change) — so we fail fast on it instead of
# burning the whole retry budget. Every other non-zero exit (a Behind
# schema while a control pod is still migrating, or a not-yet-ready
# Postgres) is transient and retried.
attempts=30
sleep_s=1
attempt=0
while :; do
    attempt=$((attempt + 1))
    # `|| rc=$?` keeps `set -e` from tripping on the expected failure and
    # captures migrate's real exit code (an `if ...; then` would reset $?).
    rc=0
    triagefactory migrate up || rc=$?
    if [ "$rc" -eq 0 ]; then
        break
    fi
    if [ "$rc" -eq 3 ]; then
        echo "migrate up: connected schema is AHEAD of this build — an executor cannot run against a newer schema. Deploy a newer executor image first (drain-first). Not retrying." >&2
        exit 1
    fi
    if [ "$attempt" -ge "$attempts" ]; then
        echo "migrate up failed after ${attempts} attempts; giving up." >&2
        exit 1
    fi
    echo "migrate up failed (attempt ${attempt}/${attempts}, exit ${rc}); retrying in ${sleep_s}s..." >&2
    sleep "$sleep_s"
done

# --- 2. Privilege separation: exec-time capability drop --------------------
#
# In multi mode on Linux — this container's sandbox is Linux-only and this
# whole mechanism exists to protect it — privilege separation is the only
# sandbox launch path on the sandbox-hosting role (executor; multi mode
# refuses to boot the fused `all` shape). This entrypoint:
#
#   1. Spawns the cap-broker in the background — UNLESS this is a control
#      pod (control_role below), which never launches a sandbox and so
#      skips the broker outright, the Go side role-gating it to match. On
#      the executor, the broker stays root, holding whatever this
#      container was granted (SYS_ADMIN + NET_ADMIN on top of Docker's
#      own baseline set — see docker-compose.yml's cap_add comment); it
#      never touches credentials or hostile input. The control pod's
#      compose service carries no sandbox caps at all (only the executor
#      service references the hardening anchor), so the drop below has
#      nothing broker-shaped to shed there.
#   2. Waits (bounded) for the broker's control socket to come up (skipped
#      with the spawn on control).
#   3. execs — REPLACING this shell, not forking a child — into the
#      orchestrator via setpriv, which atomically clears the capability
#      bounding set, drops inheritable capabilities, sets no_new_privs,
#      and switches to a different, unprivileged uid. This step runs on
#      EVERY role, control included: dropping to the unprivileged uid is
#      correct whether or not a broker was spawned, and skipping it would
#      leave the orchestrator running as root with Docker's default caps,
#      strictly worse.
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

# control_role mirrors internal/runmode.ParseRole's normalization for
# TF_ROLE: lower-case, and trim leading/trailing whitespace only (matching
# strings.TrimSpace) — NOT fold interior whitespace, so "con trol" does not
# collapse to "control" here just as ParseRole would reject it (and fail
# boot) rather than accept it. Note this DOES trim, unlike multi_mode above:
# ParseRole trims TF_ROLE where ModeFromEnv deliberately does NOT trim
# TF_MODE, so the two normalizers differ on purpose. The trim is the POSIX
# ${var#...}/${var%...} idiom run inside a command substitution so its
# scratch variable never leaks into the parent shell (like multi_mode, this
# stays side-effect-free). A control pod never launches a sandbox — every
# delegated run executes on an executor, and the brain's own LLM work is
# toolless direct API calls — so it needs no cap-broker: below, it skips the broker spawn +
# socket wait while keeping the identical uid/capability drop (running with
# root + Docker's default caps would be strictly worse).
control_role() {
    [ "$(
        r="${TF_ROLE:-}"
        r="${r#"${r%%[![:space:]]*}"}"
        r="${r%"${r##*[![:space:]]}"}"
        printf '%s' "$r" | tr '[:upper:]' '[:lower:]'
    )" = "control" ]
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
    # default — see docs/security/privilege-separation.md.
    [ -d /data ] && chown "$TF_ORCHESTRATOR_UID:$TF_ORCHESTRATOR_GID" /data
    [ -d /opt/triagefactory/sandbox ] && chown "$TF_ORCHESTRATOR_UID:$TF_ORCHESTRATOR_GID" /opt/triagefactory/sandbox

    # File-backed secrets (NAME_FILE) arrive as bind mounts that keep the
    # host file's owner and mode — Docker Compose ignores a `file:` secret's
    # uid/gid/mode, and says so — so a key the operator keeps 0600 is
    # readable by root, which is how `migrate up` above already read it, and
    # not by the uid the exec below switches to. Give the orchestrator its
    # own copy of each one and point the variable at the copy. The mount is
    # never chown'd or chmod'd: it is the operator's file on the host. A
    # variable naming a file root cannot read is left as it is, so the
    # binary's own set-but-unreadable check reports it rather than a copy
    # failure here. The variable set is read from the environment so it
    # follows secretenv's NAME_FILE convention without a second list.
    TF_SECRET_STAGE_DIR="${TF_SECRET_STAGE_DIR:-/run/tf-secrets}"
    for var in $(awk 'BEGIN { for (k in ENVIRON) if (k ~ /^TF_[A-Z0-9_]+_FILE$/) print k }'); do
        src=$(eval "printf '%s' \"\$$var\"")
        [ -n "$src" ] && [ -r "$src" ] || continue
        [ -d "$TF_SECRET_STAGE_DIR" ] || install -d -m 0700 -o "$TF_ORCHESTRATOR_UID" -g "$TF_ORCHESTRATOR_GID" "$TF_SECRET_STAGE_DIR"
        install -m 0400 -o "$TF_ORCHESTRATOR_UID" -g "$TF_ORCHESTRATOR_GID" "$src" "$TF_SECRET_STAGE_DIR/$var"
        export "$var=$TF_SECRET_STAGE_DIR/$var"
    done

    # The control role skips the broker entirely (see control_role above):
    # it never launches a sandbox, and the Go side role-gates the broker to
    # match (internal/app/privsep.go), so a broker here would be dead weight
    # the orchestrator never dials. Every other role (all, executor) spawns
    # and waits for it exactly as before — this branch is the only
    # difference, so those paths stay byte-identical.
    if ! control_role; then
        triagefactory cap-broker --socket "$TF_CAPBROKER_SOCKET" --orchestrator-uid "$TF_ORCHESTRATOR_UID" --orchestrator-gid "$TF_ORCHESTRATOR_GID" &
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
    fi

    # setpriv switches uid/gid/capabilities but never touches environment
    # variables, so HOME (inherited from this shell's own environment —
    # /root, the container runtime's default for the root user) has to be
    # overridden explicitly here. Without this, os.UserHomeDir() in the
    # orchestrator (which Go resolves purely from $HOME on Linux, with no
    # /etc/passwd fallback) would still resolve to /root — a directory
    # the now-unprivileged orchestrator uid cannot read or write.
    #
    # Note this HOME carries NO tenant content: sandboxed agents run
    # with HOME=/work (their ~/.claude session state lives inside each
    # run's org-scoped directory, resolved host-side via
    # worktree.ClaudeProjectDir — TFAC-109), and the filesystem skill
    # scan is local-mode-only. This export exists for incidental $HOME
    # lookups (git defaults, toolchain caches, libraries).
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
