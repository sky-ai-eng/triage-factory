#!/usr/bin/env bash
# Run the sandbox concurrency benchmark (cmd/sandbox-bench) inside the
# TF runtime container image.
#
# The bench must run where production sandboxes run: an environment
# with runsc + iptables + iproute2, the SYS_ADMIN/NET_ADMIN caps, the
# tool-host binary at /opt/tf/bin/tf-harness-tools, and the SDK at the
# baked toolchain root — i.e. the runtime image. Running it in a
# container also keeps all netns/iptables/bundle churn off the host —
# everything dies with the container.
#
# No TF orchestrator runs here: the bench binary spawns its own
# cap-broker (re-exec'ing itself with the `cap-broker` subcommand, the
# same fallback the production binary uses on bare metal), so replacing
# the image entrypoint is fine — the brokered launch path is intact.
#
# The image is built with plain `docker build` (not compose) so the
# bench needs no .env / Postgres / GoTrue. A dedicated rootfs cache
# volume keeps the alpine download + apk bake warm across runs; the
# first ever run pays it once (~5-10 min).
#
# Usage:
#   ./scripts/sandbox-bench.sh [sandbox-bench flags...]
#   ./scripts/sandbox-bench.sh -profile native-idle -levels 8,16,32 -hold 30s
#
# Results (CSV + capacity-model JSON) land in ./bench-results/ on the host.
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE=triagefactory-bench
OUT_DIR="$PWD/bench-results"
mkdir -p "$OUT_DIR"

echo "==> building static bench binary"
CGO_ENABLED=0 GOOS=linux go build -o "$OUT_DIR/sandbox-bench" ./cmd/sandbox-bench

echo "==> building runtime image ($IMAGE)"
docker build -t "$IMAGE" -f docker/Dockerfile .

# Bench-dedicated rootfs cache volume, kept warm across bench runs. (The
# compose stack's executors use anonymous per-replica volumes, so there is
# no shared named volume to borrow.)
ROOTFS_VOL=tf-bench-rootfs
docker volume create "$ROOTFS_VOL" >/dev/null
echo "==> rootfs cache volume: $ROOTFS_VOL"

STAMP=$(date +%Y%m%d-%H%M%S)
CSV="bench-$STAMP.csv"
JSON="bench-$STAMP.json"
echo "==> running bench (results: bench-results/$CSV + $JSON)"
docker run --rm --init \
    --name tf-sandbox-bench \
    --cgroupns=private \
    --cap-add SYS_ADMIN --cap-add NET_ADMIN \
    --security-opt apparmor=unconfined --security-opt seccomp=unconfined \
    -v "$ROOTFS_VOL":/opt/triagefactory/sandbox \
    -v "$OUT_DIR/sandbox-bench":/usr/local/bin/sandbox-bench:ro \
    -v "$OUT_DIR":/bench-out \
    --entrypoint /usr/local/bin/sandbox-bench \
    "$IMAGE" \
    -csv "/bench-out/$CSV" -json "/bench-out/$JSON" "$@"

echo "==> done: bench-results/$CSV + bench-results/$JSON"
