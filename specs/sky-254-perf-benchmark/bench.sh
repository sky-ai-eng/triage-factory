#!/bin/sh
# gVisor overhead benchmark — runs inside the Fly Machine probe.
#
# Measures two numbers that bracket the perf profile:
#   1. Cold-start cost: time to spin up a sandbox and run /bin/true.
#      Load-bearing for short-call workloads (scorer, classifier) where
#      startup dominates wall-clock.
#   2. Syscall-heavy workload: find / -type f over the alpine rootfs.
#      Stat + getdents heavy; approximates the agent's file-ops profile
#      (Read/Write/Edit/Grep tools all syscall through similar paths).
#
# Each scenario × platform × 10 iterations. Output is CSV to stdout so
# the host side can collect with fly ssh console.
set -eu

BUNDLE=/tmp/bench-bundle
ALPINE_TGZ=/tmp/alpine.tgz

# ----------------------------------------------------------------------
# Setup: alpine rootfs + default OCI spec. Reuses the same alpine 3.20
# minirootfs the production sandbox will use, so the syscall surface
# matches what real agent workloads will hit.
# ----------------------------------------------------------------------
hr() { printf '\n=== %s ===\n' "$*" >&2; }

hr "Setup: extracting alpine rootfs"
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/rootfs"
if [ ! -f "$ALPINE_TGZ" ]; then
    curl -fsSL -o "$ALPINE_TGZ" \
        https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/x86_64/alpine-minirootfs-3.20.3-x86_64.tar.gz
fi
tar -xzf "$ALPINE_TGZ" -C "$BUNDLE/rootfs"
( cd "$BUNDLE" && runsc spec )

# Count files in the alpine rootfs so the syscall workload has a
# known target. Should be ~4000 files for the 3.20 minirootfs.
ROOTFS_FILES=$(find "$BUNDLE/rootfs" -type f 2>/dev/null | wc -l)
echo "# rootfs has $ROOTFS_FILES files" >&2

# ----------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------

# patch_spec_cmd <cmd...> — rewrites process.args in the OCI spec.
patch_spec_cmd() {
    python3 - "$@" <<'PYEOF'
import json, sys
p = "/tmp/bench-bundle/config.json"
spec = json.load(open(p))
spec["process"]["args"] = sys.argv[1:]
spec["process"]["terminal"] = False
# Strip the default capabilities to drop a small noise source —
# we're measuring sandbox runtime cost, not cap-application cost.
json.dump(spec, open(p, "w"))
PYEOF
}

# elapsed <command...> — runs command, prints elapsed seconds to stdout.
# Uses /usr/bin/time -f '%e' which outputs to stderr as a float.
elapsed() {
    /usr/bin/time -f '%e' "$@" 2>&1 >/dev/null | tail -1
}

# csv_row <scenario> <platform> <run> <seconds>
csv_row() {
    printf '%s,%s,%s,%s\n' "$1" "$2" "$3" "$4"
}

# bench_direct <scenario> <iter> <cmd...>
bench_direct() {
    scenario=$1
    shift
    iter=$1
    shift
    for i in $(seq 1 "$iter"); do
        t=$(elapsed "$@")
        csv_row "$scenario" "direct" "$i" "$t"
    done
}

# bench_sandboxed <scenario> <platform> <iter> — uses whatever's in
# the bundle's process.args (set via patch_spec_cmd before calling).
bench_sandboxed() {
    scenario=$1
    platform=$2
    iter=$3
    for i in $(seq 1 "$iter"); do
        # Unique container ID per invocation; runsc requires it.
        id="bench-${scenario}-${platform}-${i}-$$"
        t=$(elapsed runsc \
            --platform="$platform" \
            --ignore-cgroups \
            --network=none \
            run \
            --bundle "$BUNDLE" \
            "$id")
        csv_row "$scenario" "$platform" "$i" "$t"
    done
}

# ----------------------------------------------------------------------
# Output header
# ----------------------------------------------------------------------
echo "scenario,platform,run,seconds"

ITER=10

# ----------------------------------------------------------------------
# Scenario 1: cold-start (/bin/true)
# ----------------------------------------------------------------------
hr "Scenario 1/2: cold-start (/bin/true)"

bench_direct "coldstart" "$ITER" /bin/true

patch_spec_cmd /bin/true
bench_sandboxed "coldstart" "ptrace" "$ITER"
bench_sandboxed "coldstart" "systrap" "$ITER"

# ----------------------------------------------------------------------
# Scenario 2: syscall-heavy workload (dd 100k tiny writes)
# ----------------------------------------------------------------------
# dd with bs=1 count=100000 generates ~100k write() syscalls + ~100k
# read() syscalls. This isolates *sustained* syscall overhead from
# cold-start cost — total runtime is dominated by syscall throughput,
# not by sandbox spawn. Subtracting the cold-start number from the
# total gives per-syscall overhead.
#
# 100k is calibrated so the direct baseline measures > 10ms (above
# our 0.01s timing precision) but stays under a few seconds. On a
# Fly shared-cpu-1x this lands around 80-200ms direct.
hr "Scenario 2/2: syscall-heavy (dd 100k writes)"

DD_CMD="dd if=/dev/zero of=/dev/null bs=1 count=100000"

# Direct: 100k writes on the host kernel
bench_direct "dd_100k" "$ITER" sh -c "$DD_CMD 2>/dev/null"

# Sandboxed: 100k writes go through gVisor's syscall path
patch_spec_cmd /bin/sh -c "$DD_CMD 2>/dev/null"
bench_sandboxed "dd_100k" "ptrace" "$ITER"
bench_sandboxed "dd_100k" "systrap" "$ITER"

hr "Done"
