#!/usr/bin/env bash
# Triage Factory — PR line breakdown for the pull request template.
#
# Counts the lines this branch changes, split into the Code / Tests /
# Documentation / Comments buckets .github/pull_request_template.md asks for,
# and prints a markdown table ready to paste into it.
#
#   ./scripts/pr-lines.sh                      # HEAD vs the resolved base
#   ./scripts/pr-lines.sh | pbcopy             # table only — diagnostics go to stderr
#   ./scripts/pr-lines.sh --worktree           # include uncommitted tracked changes
#   ./scripts/pr-lines.sh --base origin/aa/TFAC-123   # stacked on another branch
#   ./scripts/pr-lines.sh --explain            # show how each file was bucketed
#   ./scripts/pr-lines.sh --json               # machine-readable, for agents
#   ./scripts/pr-lines.sh --help               # every flag
#
# The base is resolved in this order: --base, then the open PR for this branch
# (via gh, when it's installed and authed), then git config
# branch.<name>.tfBase, then the repository's default branch. The diff is taken
# from the MERGE BASE of that ref and HEAD — the same three-dot comparison
# GitHub shows under "Files changed" — so a branch that predates recent work on
# main still reports only its own lines.
#
# This is a thin wrapper; the tool itself is scripts/prlines/.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

if ! command -v go >/dev/null 2>&1; then
  echo "pr-lines: needs the Go toolchain on PATH" >&2
  exit 1
fi

exec go run ./scripts/prlines "$@"
