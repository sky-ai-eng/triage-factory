#!/usr/bin/env bash
# Run the poll-scale benchmark (cmd/pollbench): a cold-start GitHub sync
# against a deterministic fake GitHub API at large-org shape, driven through
# the real poller + tracker. See docs/benchmarks/poll-bench.md for the standard shapes
# and recorded baselines.
#
# Unlike sandbox-bench.sh this needs no container: the fake GitHub server,
# the in-memory SQLite state, and the poller all live in one Go process.
#
# Usage:
#   ./scripts/poll-bench.sh --shape dp1
#   ./scripts/poll-bench.sh --shape smoke --rate-limit 50 --rate-limit-window 30s
#   ./scripts/poll-bench.sh --repos 100 --prs-mean 30 --latency 60ms
#
# The report prints to stdout and is archived in ./bench-results/.
set -euo pipefail

cd "$(dirname "$0")/.."

OUT_DIR="$PWD/bench-results"
mkdir -p "$OUT_DIR"

STAMP=$(date +%Y%m%d-%H%M%S)
OUT="$OUT_DIR/poll-bench-$STAMP.txt"

echo "==> running poll bench (report: bench-results/poll-bench-$STAMP.txt)"
go run ./cmd/pollbench "$@" | tee "$OUT"
