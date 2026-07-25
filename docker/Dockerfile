# syntax=docker/dockerfile:1.7
#
# Triage Factory runtime image.
#
# Multi-stage:
#   1. frontend-builder — node:22-alpine, builds frontend/dist for go:embed
#   2. sdk-builder      — node:22-alpine, pre-installs the Agent SDK so
#                         EnsureSDK is a no-op at runtime (the runtime layer
#                         has no node)
#   3. go-builder       — golang:1.26-alpine, static (CGO_ENABLED=0) build
#                         of the TF binary with the embedded frontend
#   4. runtime          — alpine:3.20, ships triagefactory + runsc + apk
#                         tools + the pre-installed SDK + entrypoint.sh
#
# Same image targets both deployments:
#   - Fly.io hosted SaaS (fly.toml at repo root)
#   - Self-host docker-compose (docker-compose.yml at repo root)
#
# Multi-arch (amd64 + arm64) via buildx; TARGETARCH picks runsc + the
# runtime alpine variant. The alpine minirootfs the in-container
# sandbox extracts at first launch is selected at RUNTIME by the Go
# binary (internal/sandbox/rootfs.go alpineRootfsForArch via
# runtime.GOARCH), NOT here — this Dockerfile only fetches the right
# runsc.

# --- stage 1: frontend ------------------------------------------------------

FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend-builder
WORKDIR /src/frontend
# pnpm is provisioned via corepack, pinned by the "packageManager" field
# in frontend/package.json. COREPACK_ENABLE_DOWNLOAD_PROMPT=0 keeps the
# pinned-version download non-interactive in the build.
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
# pnpm-workspace.yaml carries the dependency overrides, and pnpm records them
# in the lockfile — so a frozen install that can't see the file reads it as a
# config/lockfile mismatch and refuses to proceed. It belongs in the same
# dependency-only layer as the manifest, ahead of the full source copy, so an
# unrelated source edit doesn't invalidate the install cache.
COPY frontend/package.json frontend/pnpm-lock.yaml frontend/pnpm-workspace.yaml ./
RUN corepack enable && pnpm install --frozen-lockfile
COPY frontend/ ./
RUN pnpm run build

# --- stage 2: Agent SDK pre-install -----------------------------------------
#
# Materializes ~/.triagefactory/sdk/ exactly as EnsureSDK would on a
# fresh local install: copies the pinned package.json + lockfile +
# wrapper.mjs from internal/agentproc, runs `npm ci`, leaves the
# result for the runtime stage to COPY in. The reorder in
# install.go's EnsureSDK (SDK presence check first, node/npm check
# second) makes the runtime layer see this as already-installed and
# skip both the install AND the node/npm presence checks.

FROM --platform=$BUILDPLATFORM node:22-alpine AS sdk-builder
WORKDIR /sdk-install
# package-lock.json is the source of truth for transitive deps.
# wrapper.mjs is the runtime shim the host binary materializes.
# install.go is consulted ONLY to extract the pin constants (the SDK
# version and the @hono/node-server override) — we don't want to
# hardcode either here and let it drift from the Go source. The greps
# below die (and fail the build) if a constant moves or changes shape,
# which is the right failure mode: better a loud build break than a
# silent runtime SDK reinstall attempt against a runtime layer that has
# no node/npm. The manifest's *shape* is guarded from the other side by
# a drift test against install.go's sdkPackageJSON.
# --ignore-scripts mirrors installSDK in install.go: this bakes
# the same pinned tree into the shipped image, so it gets the same
# supply-chain hardening against a malicious postinstall hook.
COPY internal/agentproc/package-lock.json ./package-lock.json
COPY internal/agentproc/wrapper.mjs ./wrapper.mjs
COPY internal/agentproc/install.go /tmp/install.go
RUN set -eu; \
    SDK_VERSION=$(grep -E '^const sdkVersion = "[^"]+"' /tmp/install.go | sed -E 's/.*"([^"]+)".*/\1/'); \
    if [ -z "$SDK_VERSION" ]; then \
        echo "Could not extract sdkVersion from internal/agentproc/install.go" >&2; \
        exit 1; \
    fi; \
    HONO_OVERRIDE=$(grep -E '^const honoNodeServerOverride = "[^"]+"' /tmp/install.go | sed -E 's/.*"([^"]+)".*/\1/'); \
    if [ -z "$HONO_OVERRIDE" ]; then \
        echo "Could not extract honoNodeServerOverride from internal/agentproc/install.go" >&2; \
        exit 1; \
    fi; \
    echo "Using sdkVersion=${SDK_VERSION} (from internal/agentproc/install.go)"; \
    printf '{\n  "name": "triagefactory-sdk-runtime",\n  "private": true,\n  "type": "module",\n  "dependencies": {\n    "@anthropic-ai/claude-agent-sdk": "%s"\n  },\n  "overrides": {\n    "@hono/node-server": "%s"\n  }\n}\n' "$SDK_VERSION" "$HONO_OVERRIDE" > package.json; \
    npm ci --no-audit --no-fund --omit=dev --ignore-scripts; \
    rm -f /tmp/install.go

# --- stage 3: static Go binary ----------------------------------------------

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src
# Pre-download deps so the source-copy layer rebuilds fast on edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
# embed.go pulls frontend/dist/ into the binary; ship it in before build.
COPY --from=frontend-builder /src/frontend/dist ./frontend/dist
# Static build for alpine (musl) runtime. The same binary is also
# bind-mounted into the gVisor sandbox's alpine rootfs at runtime, so
# musl-static is the only correct shape — a glibc-linked binary would
# fail to exec in either alpine container.
ARG TARGETOS=linux
ARG TARGETARCH
# Enterprise public-key OVERRIDE (a dev/fork issuer key); empty keeps ee.publicKeyB64's
# source default. The -X is appended only when set — an unconditional -X with an empty
# value would stomp that default and silently produce an image that ignores every license.
ARG TF_LICENSE_PUBLIC_KEY=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    LDFLAGS="-s -w"; \
    if [ -n "${TF_LICENSE_PUBLIC_KEY}" ]; then \
      LDFLAGS="$LDFLAGS -X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=${TF_LICENSE_PUBLIC_KEY}"; \
    fi; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="$LDFLAGS" -o /out/triagefactory .

# --- stage 3b: compiled tool host -------------------------------------------
#
# The resident tool host the native runtime launches as the jail's main
# process. Built here rather than fetched: it is our own source, and the
# broker's argv pin names this exact path, so the image is the only place it
# can come from.
#
# musl-static for the same reason the Go binary is: it is bind-mounted into
# the sandbox's alpine rootfs and must exec there. Building ON alpine with
# the musl target gives that without a cross-toolchain.
FROM --platform=$BUILDPLATFORM rust:1.85-alpine AS toolhost-builder
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY harness ./harness
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/harness/target \
    cd harness/tf-harness-tools && \
    cargo build --release --bin tf-harness-tools && \
    mkdir -p /out && \
    cp /src/harness/target/release/tf-harness-tools /out/tf-harness-tools

# --- stage 4: runtime -------------------------------------------------------

FROM alpine:3.20 AS runtime

# - iptables / iproute2: sandbox netns + NAT POSTROUTING setup
# - ca-certificates: outbound TLS from the TF process (GitHub, Jira,
#   Anthropic) and from in-sandbox tools (when they reach back to the
#   host for cached rootfs / pre-installed SDK).
# - tini: PID 1 reaper so signal handling (Ctrl-C in interactive runs,
#   compose stop) propagates cleanly to the Go binary.
# - tzdata: timestamps in logs render in operator-local time when TZ is set.
# - curl: healthcheck probes in compose/fly + the runsc fetch above.
# - git: host-side worktree prep. The TF process shells out to git
#   (worktree.EnsureBareClone bare-clone cache + `git worktree add`
#   per run — internal/worktree) to stage a tree before bind-mounting
#   it into the gVisor sandbox. A hard runtime dependency of the host
#   process in multi-mode, distinct from (and mirroring) the in-sandbox
#   git that internal/sandbox/rootfs.go installs for the jailed agent.
# - openssh-client: the worktree layer has an SSH path
#   (internal/worktree/ssh.go) used for SSH clone URLs / GHE remotes.
# - setpriv: the entrypoint's exec-time capability drop — clears the
#   orchestrator's capability bounding set and switches it to a
#   different uid at the exec boundary, before the Go runtime ever
#   starts a thread. Never done in-process (capset/prctl from Go only
#   affects one thread and would leave others still privileged), so
#   this small, single-purpose, single-threaded C tool is a hard
#   runtime dependency of docker/entrypoint.sh's multi-mode
#   privilege-separation path.
RUN apk add --no-cache \
    iptables \
    iproute2 \
    ca-certificates \
    tini \
    tzdata \
    curl \
    git \
    openssh-client \
    setpriv

# The unprivileged orchestrator identity the entrypoint drops into for
# multi mode. A fixed, dedicated,
# non-root uid/gid — distinct from internal/sandbox.WorktreeUID (10000,
# the uid *inside* the gVisor sandbox the agent runs as) and from the
# cap-broker (which stays root) — so the two host-level privsep
# processes are never the same uid: the orchestrator, holding zero
# capabilities after the drop, cannot ptrace a same-uid broker if it
# were ever the same identity. `-D` skips a password.
#
# Unlike a purely-service identity, this one DOES need a real, owned
# home directory — but NOT for tenant content. In multi mode the
# sandboxed agents (curator + delegated runs) execute with HOME=/work,
# so their ~/.claude session state lands inside each run's own
# org-scoped directory on the data volume; host-side consumers resolve
# it via worktree.ClaudeProjectDir (TFAC-109), never this HOME. What
# still needs a valid orchestrator HOME is process plumbing that
# resolves os.UserHomeDir() incidentally (Go toolchain caches, git
# defaults, any library that touches $HOME). The entrypoint's exec-time
# drop switches uid via setpriv, which does not touch environment
# variables, so it also explicitly exports HOME=/home/tf-orchestrator
# right before that exec — without both the directory (owned by this
# uid) and the env var (setpriv won't derive it from /etc/passwd),
# those incidental lookups would point at the root-owned $HOME
# inherited from the container's default environment, which this uid
# cannot reach post-drop.
#
# tf-sandbox (gid 10000 — internal/sandbox.WorktreeGID) exists so the
# orchestrator can grant the sandbox identity access to per-run agenthost
# sockets WITHOUT CAP_CHOWN: giving a file away to another uid always
# needs that capability, but an owner may chgrp its own file to any group
# it is a member of. Making tf-orchestrator a member (and having the
# entrypoint's setpriv carry exactly this one supplementary group through
# the drop) turns the socket hand-off into an owner-legal chgrp + 0660 —
# see cmd/exec/agenthost/socket_linux.go's grantSocketToSandbox. No host
# process runs AS this gid; it exists purely as the grant target matching
# the in-sandbox agent's gid.
RUN addgroup -g 10000 tf-sandbox \
    && addgroup -g 10001 tf-orchestrator \
    && adduser -D -u 10001 -G tf-orchestrator -h /home/tf-orchestrator -s /sbin/nologin tf-orchestrator \
    && adduser tf-orchestrator tf-sandbox

# Pull the per-arch runsc binary from a pinned gVisor release.
#
# Pinning (rather than tracking /release/latest/) keeps image builds
# reproducible: a rebuild six months from now of the same TF commit
# produces a runsc with identical behavior and identical syscall
# semantics. SHA512 is also pinned in-repo per arch and verified
# before install — a CDN compromise or accidental upstream rewrite
# fails the build loudly instead of silently shipping a different
# sandbox runtime.
#
# Bumping: pick a newer release tag from
#   https://storage.googleapis.com/storage/v1/b/gvisor/o?prefix=releases/release/&delimiter=/
# refresh both SHA512s from the sidecar files at
#   https://storage.googleapis.com/gvisor/releases/release/<VER>/<arch>/runsc.sha512
# and re-run the SKY-254 validation probe to confirm nothing in the
# syscall surface that TF depends on has changed.
ARG GVISOR_RELEASE=20260511
ARG GVISOR_RUNSC_SHA512_AMD64=e227a71c95e794119f6648a44083df945392c6cd457f36abbc49c2b6e0b87c7f01b94e6bf4632f4cb22ee34fbec7a2c34ca03d30efa9c689db76a6215a6e44e1
ARG GVISOR_RUNSC_SHA512_ARM64=1a1da6060bbfb98fcd98304a7e73afb2c23a897ae4a8bf741e1c6a16cf07e0cd14bcd9d0f0e51aff201feb0cf8d19952f944aeb5eb5d92b091509374b9702a26
ARG TARGETARCH
RUN set -eux; \
    case "$TARGETARCH" in \
        amd64) GVISOR_ARCH=x86_64; EXPECTED_SHA=$GVISOR_RUNSC_SHA512_AMD64 ;; \
        arm64) GVISOR_ARCH=aarch64; EXPECTED_SHA=$GVISOR_RUNSC_SHA512_ARM64 ;; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    base=https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${GVISOR_ARCH}; \
    curl -fsSL -o /usr/local/bin/runsc "$base/runsc"; \
    echo "${EXPECTED_SHA}  /usr/local/bin/runsc" | sha512sum -c -; \
    chmod 0755 /usr/local/bin/runsc

# TF-pinned real `gh` release binary (TFAC-669), bind-mounted read-only into the
# sandbox at /opt/tf/bin/gh (a PATH-leading dir) so the agent's gh talks to the
# per-run credential-injector proxy. Pinned + checksum-verified here, exactly
# like runsc above: a user profile image with its own gh loses the PATH race and
# would have no credentials anyway. GitHub's official linux tarballs are static,
# so this musl-alpine-jail-safe binary execs fine in the rootfs.
#
# Bumping: pick a newer tag from https://github.com/cli/cli/releases and refresh
# both SHA256s from that release's gh_<VER>_checksums.txt. Keep the version in
# step with the spike-validated surface (the injector's two path prefixes).
ARG GH_VERSION=2.96.0
ARG GH_SHA256_AMD64=83d5c2ccad5498f58bf6368acb1ab32588cf43ab3a4b1c301bf36328b1c8bd60
ARG GH_SHA256_ARM64=06f86ec7103d41993b76cd78072f43595c34aaa56506d971d9860e67140bf909
RUN set -eux; \
    case "$TARGETARCH" in \
        amd64) GH_ARCH=amd64; GH_SHA=$GH_SHA256_AMD64 ;; \
        arm64) GH_ARCH=arm64; GH_SHA=$GH_SHA256_ARM64 ;; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac; \
    tarball="gh_${GH_VERSION}_linux_${GH_ARCH}.tar.gz"; \
    curl -fsSL -o /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${tarball}"; \
    echo "${GH_SHA}  /tmp/gh.tgz" | sha256sum -c -; \
    mkdir -p /opt/tf/bin; \
    tar -xzf /tmp/gh.tgz -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${GH_ARCH}/bin/gh" /opt/tf/bin/gh; \
    rm -rf /tmp/gh.tgz "/tmp/gh_${GH_VERSION}_linux_${GH_ARCH}"; \
    /opt/tf/bin/gh --version

# Pre-installed Agent SDK. EnsureSDK resolves it via paths.SDKDir() =
# <ToolchainRoot>/sdk, and ToolchainRoot honors TF_TOOLCHAIN_ROOT (set
# below), so the binary finds this baked copy at /opt/triagefactory/sdk
# in every mode — decoupled from wherever TF_STATE_ROOT points tenant
# data. The runtime layer has no node/npm, so this bake is the ONLY copy
# the sandbox ever gets.
COPY --from=sdk-builder /sdk-install /opt/triagefactory/sdk

# Pin the host-global toolchain root to the baked path above so BOTH the
# default local mode and multi mode (TF_MODE=multi at deploy) resolve the
# SDK + sandbox rootfs cache here, never under TF_STATE_ROOT (which
# operators repoint at a tenant-data volume). See internal/paths.ToolchainRoot.
ENV TF_TOOLCHAIN_ROOT=/opt/triagefactory

# The TF binary itself.
COPY --from=go-builder /out/triagefactory /usr/local/bin/triagefactory

# The compiled tool host, at the fixed path the broker validates the native
# jail's binary mount and argv pin against (sandbox.TrustedToolHostBinaryPath /
# TrustedToolHostBinaryDestination). Same PATH-leading dir as the pinned gh.
COPY --from=toolhost-builder /out/tf-harness-tools /opt/tf/bin/tf-harness-tools
RUN chmod 0755 /opt/tf/bin/tf-harness-tools

# Entrypoint script: runs goose migrations with bounded retry against
# a not-yet-ready Postgres, then exec's the binary so tini's signal
# forwarding lands directly on it. See docker/entrypoint.sh for the
# full behavior + GoTrue-keys note.
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod 0755 /entrypoint.sh

# Default mode — overridden by Fly secrets or compose env. Local mode
# is the safe default (no Postgres, no GoTrue) so a `docker run`
# without any env vars boots into a working single-tenant binary that
# operators can poke at to validate the image. Multi-mode operators
# set TF_MODE=multi + TF_DATABASE_URL at deploy time.
ENV TF_MODE=local

EXPOSE 3000

# tini reaps zombies + forwards signals to the binary (the entrypoint
# exec's triagefactory after first-boot setup). Using -g so the whole
# process group sees SIGTERM, not just PID 1.
ENTRYPOINT ["/sbin/tini", "-g", "--", "/entrypoint.sh"]
CMD ["--host", "0.0.0.0", "--port", "3000", "--no-browser"]
