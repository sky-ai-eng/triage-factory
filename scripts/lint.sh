#!/usr/bin/env bash
# Lint + format check for Go, the Rust harness, and the frontend.
# Usage:
#   ./scripts/lint.sh           # check only, exit non-zero on issues
#   ./scripts/lint.sh --fix     # auto-fix where possible
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

FIX=0
if [[ "${1:-}" == "--fix" ]]; then
  FIX=1
fi

GOIMPORTS="${GOIMPORTS:-$(go env GOPATH)/bin/goimports}"

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
blue() { printf '\033[34m== %s ==\033[0m\n' "$*"; }

fail=0

# --- Go ---------------------------------------------------------------------
blue "goimports"
if [[ ! -x "$GOIMPORTS" ]]; then
  red "goimports not found at $GOIMPORTS — run: go install golang.org/x/tools/cmd/goimports@latest"
  fail=1
else
  # Format every tracked .go file except generated/vendored.
  go_files=$(git ls-files '*.go' | grep -v '^vendor/' || true)
  if [[ -n "$go_files" ]]; then
    if (( FIX )); then
      echo "$go_files" | xargs "$GOIMPORTS" -w
      green "goimports applied"
    else
      unformatted=$(echo "$go_files" | xargs "$GOIMPORTS" -l)
      if [[ -n "$unformatted" ]]; then
        red "Unformatted Go files:"
        echo "$unformatted"
        fail=1
      else
        green "Go formatting OK"
      fi
    fi
  fi
fi

blue "go vet"
if go vet ./...; then
  green "go vet OK"
else
  fail=1
fi

# Everything above only ever typechecks the host's own GOOS, so a symbol
# parked behind a `//go:build linux` tag and referenced from an untagged
# file passes every check on a Linux runner and breaks the build on a
# macOS laptop (and vice versa). Both are supported dev platforms — the
# sandbox is Linux-only at RUNTIME, but the whole module still has to
# compile on a Mac — so build for both and let whichever one isn't the
# host catch the tag mistake. Cross-compiles cost a one-time stdlib
# build; after that the build cache makes them cheap.
#
# `go build ./...` over multiple packages discards binaries, so this
# leaves no artifacts behind. It needs frontend/dist for the root
# package's go:embed, same as `go vet` above.
blue "cross-platform build"
for target in linux/amd64 darwin/arm64; do
  if GOOS="${target%/*}" GOARCH="${target#*/}" go build ./...; then
    green "builds for $target"
  else
    red "build failed for $target — check for a build-tagged symbol used from an untagged file"
    fail=1
  fi
done

blue "golangci-lint"
if command -v golangci-lint >/dev/null 2>&1; then
  if (( FIX )); then
    golangci-lint run --fix ./... || fail=1
  else
    golangci-lint run ./... || fail=1
  fi
else
  red "golangci-lint not installed (brew install golangci-lint)"
  fail=1
fi

# forbidigo (in .golangci.yml) bans os.UserHomeDir outside internal/paths,
# but its AST matcher can't see string literals — so the ".triagefactory"
# path literal needs this companion grep. It must catch both the
# double-quoted and the backtick raw-string forms (".triagefactory",
# `.triagefactory`, "~/.triagefactory/...") while NOT tripping on:
#   - the same path quoted inside a doc comment (a backtick-quoted path in
#     a comment is textually identical to a raw-string literal), and
#   - the triagefactory.com marketing domain in URLs.
# So: strip // line comments first, then match .triagefactory only as a
# path *segment* — flanked by a quote/backtick/slash/tilde and ending the
# segment with / or a closing quote/backtick (the domain is followed by
# ".com", so it's excluded). internal/paths owns the literal; test files
# may reference it.
blue "paths state-literal guard"
go_src=$(git ls-files '*.go' \
  | grep -v '_test\.go$' \
  | grep -v '^internal/paths/' || true)
literal_offenders=""
for f in $go_src; do
  if sed 's://.*::' "$f" | grep -qE '[`"/~]\.triagefactory[/"`]'; then
    literal_offenders+="$f"$'\n'
  fi
done
if [[ -n "$literal_offenders" ]]; then
  red '".triagefactory" path literal found outside internal/paths — use a paths.* resolver:'
  echo "$literal_offenders"
  fail=1
else
  green "paths state-literal guard OK"
fi

# Open-core boundary: the dependency graph points inward, so core (internal/,
# cmd/) must never import ee/. Only the composition root (package main, at the
# repo root) wires ee in — ee.Install() + the ee/sso blank import — so it is the
# sole allowed importer and lives outside these trees. We check Imports AND
# Test/XTestImports: a core *test* reaching into ee (the old in-package
# test-stub wiring) is exactly the regression this blocks.
blue "ee import boundary guard"
ee_pkg="github.com/sky-ai-eng/triage-factory/ee"
# Run go list as its own step and check its exit status: suppressing the error
# (2>/dev/null + || true) would let a go list failure (build break, bad build
# tag) masquerade as "no offenders" and report OK without verifying anything.
# Stderr flows to the console so any failure is visible; the `if !` keeps
# set -e from aborting before we can report it.
if ! ee_pkg_imports=$(go list -f '{{$ip := .ImportPath}}{{range .Imports}}{{$ip}} {{.}}
{{end}}{{range .TestImports}}{{$ip}} {{.}}
{{end}}{{range .XTestImports}}{{$ip}} {{.}}
{{end}}' ./internal/... ./cmd/...); then
  red "ee import boundary guard: 'go list' failed (see above) — boundary NOT verified"
  fail=1
elif [[ -z "${ee_pkg_imports//[[:space:]]/}" ]]; then
  # internal/ + cmd/ always have many packages with imports; empty output means
  # go list silently resolved nothing — treat as unverified, not as "no offenders".
  red "ee import boundary guard: 'go list' returned no packages — boundary NOT verified"
  fail=1
else
  ee_offenders=$(printf '%s\n' "$ee_pkg_imports" \
    | awk -v ee="$ee_pkg" '$2 == ee || index($2, ee"/") == 1 {print $1" imports "$2}' \
    | sort -u)
  if [[ -n "$ee_offenders" ]]; then
    red "core (internal/, cmd/) must not import ee/ — the open-core boundary points inward; only package main may wire ee:"
    echo "$ee_offenders"
    fail=1
  else
    green "ee import boundary guard OK"
  fi
fi

# --- Rust (harness/) ---------------------------------------------------------
# The in-sandbox tool binaries. CI gates on fmt + clippy-as-errors, and until
# this section existed there was no local equivalent — so a warning-clean Go
# and frontend tree still read as "all checks passed" while CI failed on an
# unused import. A missing toolchain is a hard failure for the same reason
# golangci-lint's is: a skip silently downgrades this script's contract from
# "CI will pass" to "the parts I could check will pass".
blue "cargo fmt + clippy"
CARGO="${CARGO:-$(command -v cargo || echo "$HOME/.cargo/bin/cargo")}"
if [[ ! -x "$CARGO" ]]; then
  red "cargo not found — install rust (https://rustup.rs) or set CARGO=/path/to/cargo"
  fail=1
else
  pushd harness >/dev/null
  if (( FIX )); then
    "$CARGO" fmt || fail=1
    green "cargo fmt applied"
  else
    "$CARGO" fmt --check || fail=1
  fi
  # Check-only even under --fix: `clippy --fix` rewrites source and wants a
  # clean tree, which is a bigger promise than the rest of --fix makes.
  "$CARGO" clippy --all-targets -- -D warnings || fail=1
  popd >/dev/null
fi

# --- Frontend ---------------------------------------------------------------
cd frontend

# pnpm isn't bundled with Node the way npm is, and `pnpm exec` needs deps
# already installed — fail with an actionable message instead of a bare
# "command not found" from the tools below.
if ! command -v pnpm >/dev/null 2>&1; then
  corepack enable >/dev/null 2>&1 || true
fi
if ! command -v pnpm >/dev/null 2>&1; then
  red "pnpm not found — run: corepack enable (version pinned in frontend/package.json)"
  exit 1
fi
if [[ ! -d node_modules ]]; then
  red "frontend deps not installed — run: cd frontend && pnpm install"
  exit 1
fi

blue "prettier"
if (( FIX )); then
  pnpm exec prettier --write "src/**/*.{ts,tsx,css,json}"
  green "prettier applied"
else
  if ! pnpm exec prettier --check "src/**/*.{ts,tsx,css,json}"; then
    fail=1
  fi
fi

blue "eslint"
if (( FIX )); then
  pnpm exec eslint . --fix || fail=1
else
  pnpm exec eslint . || fail=1
fi

blue "tsc"
if ! pnpm exec tsc -b --noEmit; then
  fail=1
fi

cd "$REPO_ROOT"

if (( fail )); then
  red "lint failed"
  exit 1
fi
green "all checks passed"
