#!/usr/bin/env bash
# SessionStart setup for Claude Code on the web.
#
# Why this exists: the Go root package go:embed's frontend/dist/* (see
# embed.go), so `go build .` and `go test ./...` FAIL with
# "pattern frontend/dist/*: no matching files found" until the frontend
# has been built at least once. This hook builds the frontend and warms
# Go module + lint-tool caches so tests, builds, and lint work in a fresh
# web session. The container is snapshotted after the hook completes, so
# subsequent sessions reuse the warm state.
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
command -v golangci-lint >/dev/null 2>&1 || go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest >>"$log" 2>&1 || true

echo "[session-start] done (full log: $log)"
