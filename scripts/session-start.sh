#!/usr/bin/env bash
# SessionStart setup for Claude Code on the web.
#
# Why this exists: the Go root package go:embed's frontend/dist/* (see
# embed.go), so `go build .` and `go test ./...` FAIL with
# "pattern frontend/dist/*: no matching files found" until the frontend
# has been built at least once. This hook builds the frontend, starts the
# Docker daemon (needed by internal/db/pgtest's testcontainers-backed
# Postgres tests) and force-updates golangci-lint, and warms Go module
# caches so tests, builds, and lint work in a fresh web session. The
# container is snapshotted after the hook completes, so subsequent
# sessions reuse the warm state.
#
# Lives in scripts/ (tracked) rather than .claude/hooks/ because the repo
# gitignores .claude/* except settings.json — settings.json points here.
set -euo pipefail

# Web/remote sessions only — local machines already have their toolchain.
[ "${CLAUDE_CODE_REMOTE:-}" != "true" ] && exit 0

# Async: return immediately so the session starts fast; finish setup in the
# background (asyncTimeout caps it at 5m — the work above takes ~1m). Trade-off:
# there's a brief window where a build/test could run before frontend/dist and
# the caches are ready, so an early `go build .` may still hit the embed error
# until this completes.
echo '{"async": true, "asyncTimeout": 300000}'

cd "${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
log=/tmp/tf-session-start.log
: > "$log"

echo "[session-start] go mod download"
go mod download >>"$log" 2>&1

# Docker: the daemon isn't started by default in this sandbox even though the
# CLI is baked in, and internal/db/pgtest (testcontainers-go) needs a live
# daemon to run Postgres tests instead of skipping them. Docker Hub's
# blob-storage CDN (production.cloudfront.docker.com) is blocked by this
# environment's egress proxy, so pulls 403 even once the daemon is up —
# mirror.gcr.io (Google's public Docker Hub pull-through cache) routes through
# allowed infra and works. `nohup ... & disown` is required, not a plain `&`:
# each hook invocation is a short-lived process, so a plain background job
# gets reaped when this script exits.
echo "[session-start] docker: registry mirror + daemon"
{
  if ! docker info >/dev/null 2>&1; then
    mkdir -p /etc/docker
    cat > /etc/docker/daemon.json <<'JSON'
{
  "registry-mirrors": ["https://mirror.gcr.io"]
}
JSON
    nohup dockerd >/tmp/dockerd.log 2>&1 < /dev/null &
    disown
  fi
} || true

# pnpm isn't bundled with Node the way npm is — provision it via Corepack
# (version pinned by frontend/package.json's packageManager field). Best-effort
# under set -e: if the shim dir isn't writable the build below surfaces it.
echo "[session-start] provisioning pnpm via corepack"
corepack enable >>"$log" 2>&1 || true

# REQUIRED: creates frontend/dist for the go:embed above. Without this the
# root package (and therefore `go test ./...` / `go build .`) does not compile.
echo "[session-start] frontend: pnpm install + vite build (creates frontend/dist)"
( cd frontend && pnpm install --frozen-lockfile && pnpm run build ) >>"$log" 2>&1

# Best-effort: scripts/lint.sh + the repo's PostToolUse formatter need these.
echo "[session-start] go tooling (goimports, golangci-lint)"
gobin="$(go env GOPATH)/bin"
[ -x "$gobin/goimports" ] || go install golang.org/x/tools/cmd/goimports@latest >>"$log" 2>&1 || true

# Force-update rather than skip-if-present: the base image bakes in a
# golangci-lint that goes stale over time (v2.5.0 as of this writing vs.
# latest v2.12.x), and `command -v` would keep finding that stale one at
# /usr/local/bin forever since GOPATH/bin isn't on PATH. `go install pkg@latest`
# resolves the Go toolchain from pkg's own go.mod, not this repo's — pin
# GOTOOLCHAIN to this repo's go.mod version so the binary isn't built with an
# older Go than go.mod targets (golangci-lint refuses to run in that case).
# Installing straight to /usr/local/bin overwrites the baked-in copy in place.
echo "[session-start] go tooling (golangci-lint force-update to latest)"
go_ver="$(awk '/^go /{print $2; exit}' go.mod)"
GOTOOLCHAIN="go${go_ver}" GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest >>"$log" 2>&1 || true

# Chromium is pre-installed at $PLAYWRIGHT_BROWSERS_PATH and already works with
# zero config (chromium.launch() with no args finds it). This just guards a
# hypothetical future `@playwright/test` devDependency in frontend/package.json
# from having its postinstall redownload a browser that's already there.
echo 'export PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1' >> "${CLAUDE_ENV_FILE:-/dev/null}"

if docker info >/dev/null 2>&1; then
  echo "[session-start] docker daemon ready"
else
  echo "[session-start] docker daemon not ready yet — pg tests will skip cleanly"
fi

echo "[session-start] done (full log: $log)"
