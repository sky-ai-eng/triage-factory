#!/usr/bin/env bash
# Lint + format check for both Go and frontend.
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
# path literal needs this companion grep. Every persistent TF state path
# must come from a paths.* resolver, never a hardcoded literal.
# internal/paths owns the literal; internal/curator/knowledge.go is a
# temporary exception until SKY-402 routes the curator runtime through
# paths; test files may reference it for assertions.
blue "paths state-literal guard"
go_src=$(git ls-files '*.go' \
  | grep -v '_test\.go$' \
  | grep -v '^internal/paths/' \
  | grep -v '^internal/curator/knowledge\.go$' || true)
literal_offenders=""
if [[ -n "$go_src" ]]; then
  literal_offenders=$(printf '%s\n' "$go_src" | xargs grep -l '"\.triagefactory' 2>/dev/null || true)
fi
if [[ -n "$literal_offenders" ]]; then
  red '".triagefactory" path literal found outside internal/paths — use a paths.* resolver:'
  echo "$literal_offenders"
  fail=1
else
  green "paths state-literal guard OK"
fi

# --- Frontend ---------------------------------------------------------------
blue "prettier"
cd frontend
if (( FIX )); then
  npx prettier --write "src/**/*.{ts,tsx,css,json}"
  green "prettier applied"
else
  if ! npx prettier --check "src/**/*.{ts,tsx,css,json}"; then
    fail=1
  fi
fi

blue "eslint"
if (( FIX )); then
  npx eslint . --fix || fail=1
else
  npx eslint . || fail=1
fi

blue "tsc"
if ! npx tsc -b --noEmit; then
  fail=1
fi

cd "$REPO_ROOT"

if (( fail )); then
  red "lint failed"
  exit 1
fi
green "all checks passed"
