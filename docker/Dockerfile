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
COPY frontend/package.json frontend/pnpm-lock.yaml ./
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
# install.go is consulted ONLY to extract the sdkVersion constant —
# we don't want to hardcode the version here and let it drift from
# the Go source. The grep below dies (and fails the build) if the
# constant moves or changes shape, which is the right failure mode:
# better a loud build break than a silent runtime SDK reinstall
# attempt against a runtime layer that has no node/npm.
# --ignore-scripts mirrors installSDKIfNeeded in install.go: this bakes
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
    echo "Using sdkVersion=${SDK_VERSION} (from internal/agentproc/install.go)"; \
    printf '{\n  "name": "triagefactory-sdk-runtime",\n  "private": true,\n  "type": "module",\n  "dependencies": {\n    "@anthropic-ai/claude-agent-sdk": "%s"\n  }\n}\n' "$SDK_VERSION" > package.json; \
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
# Enterprise PUBLIC key, baked so official images verify licenses. Public (it ships in
# every binary); empty = community image, EE off. Passed as a build-arg by docker-publish.yml.
ARG TF_LICENSE_PUBLIC_KEY=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/sky-ai-eng/triage-factory/ee.publicKeyB64=${TF_LICENSE_PUBLIC_KEY}" -o /out/triagefactory .

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
RUN apk add --no-cache \
    iptables \
    iproute2 \
    ca-certificates \
    tini \
    tzdata \
    curl \
    git \
    openssh-client

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
