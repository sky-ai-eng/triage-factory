#!/usr/bin/env bash
# Triage Factory — Go dependency license gate.
#
# We distribute the TF binary (it statically links its whole Go dependency
# tree), and as of v1.13 we ship a self-host stack to customers. This gate
# fails if any dependency in the distributed binary carries a copyleft /
# source-available license that would impose obligations we don't intend to
# take on (e.g. an AGPL network-copyleft clause that would land on
# self-hosters). It's the single source of truth for the license check: run by
# .github/workflows/dependency-licenses.yml on PRs + main, and as a hard prerequisite of
# the release job (.github/workflows/release.yml) so a tag can't ship binaries
# without it passing.
#
# Policy: permissive (MIT / BSD / Apache-2.0 / ISC / MPL-2.0) is fine.
# "restricted" (GPL / LGPL / AGPL) and "forbidden" fail the gate. go-licenses
# check by itself only fails on those TYPES — it merely logs a license it can't
# classify and still exits 0 — so this script ADDS a fail-closed step: any
# unclassifiable, non-ignored dependency also fails the gate. A novel
# source-available license (SSPL, BSL) isn't SPDX-standard and won't classify,
# so that step is what catches it. A genuinely-permissive dep the (dated) v1.6.0
# classifier can't match must be verified by hand and added to IGNORE below with
# a one-line justification, NOT waved through.
#
#   ./scripts/license-check.sh            # run the gate (exit non-zero on violation)
#   ./scripts/license-check.sh --report   # print the full module,url,license CSV
#
# --report is the on-demand inventory: run it any time you need the current list
# of bundled Go modules and their licenses (e.g. for a vendor / legal review).
# There's deliberately no checked-in inventory file — it would drift from the
# module graph and churn commit history; this gate plus --report keep the source
# of truth in the code, not a doc.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

GO_LICENSES_VERSION=v1.6.0
DISALLOWED_TYPES=forbidden,restricted

# Package-path prefixes go-licenses should skip. KEEP THIS LIST SHORT and
# justify every entry — each one is a hole in the gate.
IGNORE=(
  # First-party code: our own packages have no per-package LICENSE file; the
  # repo root is licensed under the Triage Factory License (see LICENSE) and
  # isn't a third-party redistribution concern.
  github.com/sky-ai-eng/triage-factory
  # BSD-3-Clause, verified by reading modernc.org/mathutil@v1.7.1/LICENSE (the
  # canonical 3-clause text). go-licenses v1.6.0's classifier can't match its
  # "The mathutil Authors" header, so without this entry the fail-closed step
  # below would reject it.
  modernc.org/mathutil
)

command -v go >/dev/null 2>&1 || { echo "go not found on PATH" >&2; exit 1; }

# go install drops the binary in $GOBIN when set, else the first GOPATH entry's
# bin (GOPATH can be a colon-separated list). Resolve it the same way so we find
# the installed go-licenses instead of reinstalling / looking in the wrong place.
GOBIN_DIR=$(go env GOBIN)
[ -n "$GOBIN_DIR" ] || GOBIN_DIR="$(go env GOPATH | cut -d: -f1)/bin"
GO_LICENSES="$GOBIN_DIR/go-licenses"
if ! [ -x "$GO_LICENSES" ]; then
  echo "installing go-licenses@$GO_LICENSES_VERSION..."
  go install "github.com/google/go-licenses@$GO_LICENSES_VERSION"
fi

# go-licenses loads every package, including package main, which //go:embed's
# frontend/dist — so the tree won't typecheck unless dist exists. The license
# check doesn't care about dist's CONTENTS, only that the embed resolves, so
# stub it when absent (e.g. a license-only CI job that skips the frontend
# build) and remove the stub on exit. A real dist is left untouched.
created_stub=0
if [ -z "$(ls -A frontend/dist 2>/dev/null || true)" ]; then
  mkdir -p frontend/dist
  printf '<!doctype html><title>license-check stub</title>\n' > frontend/dist/index.html
  created_stub=1
fi
cleanup() { [ "$created_stub" = "1" ] && rm -rf frontend/dist; return 0; }
trap cleanup EXIT

ignore_args=()
for m in "${IGNORE[@]}"; do ignore_args+=(--ignore "$m"); done

if [ "${1:-}" = "--report" ]; then
  # On-demand inventory: CSV to stdout, go-licenses warnings / classify notes to
  # stderr so whoever runs this sees them (redirect stdout to capture just the
  # CSV). Ignored modules (IGNORE above) are intentionally absent.
  "$GO_LICENSES" report ./... "${ignore_args[@]}"
  exit 0
fi

echo "Running go-licenses check (disallowed types: $DISALLOWED_TYPES)..."
# Capture combined output: check exits non-zero on a disallowed TYPE, but only
# LOGS (and still exits 0) a license it can't classify — so we inspect the
# output for both conditions.
out=$("$GO_LICENSES" check ./... "${ignore_args[@]}" --disallowed_types="$DISALLOWED_TYPES" 2>&1) && rc=0 || rc=$?
echo "$out" | grep -vE '^W[0-9]' || true   # surface findings; drop cgo "non-Go code" warnings

if [ "$rc" -ne 0 ]; then
  echo "" >&2
  echo "license-check: FAILED — a disallowed (copyleft/forbidden) license is in the tree (see above)." >&2
  exit 1
fi

# Fail closed on anything go-licenses couldn't classify (it logs these but still
# exits 0). A source-available license (SSPL, BSL) won't classify, so this is
# what stops one slipping through as a silent pass. Ignored modules are skipped
# entirely, so a verified-permissive exception in IGNORE never reaches here.
if echo "$out" | grep -q "Failed to find license"; then
  echo "" >&2
  echo "license-check: FAILED — unclassifiable license(s) below. Verify each by hand, then either" >&2
  echo "add it to IGNORE in this script (if permissive) or remove the dependency:" >&2
  echo "$out" | grep "Failed to find license" >&2
  exit 1
fi

echo "license-check: OK — no forbidden/restricted, and nothing unclassifiable, in the Go dependency tree"
