#!/usr/bin/env bash
# Run the sandbox concurrency benchmark (cmd/sandbox-bench) inside the
# TF runtime container image.
#
# The bench must run where production sandboxes run: an environment
# with runsc + iptables + iproute2 and the SYS_ADMIN/NET_ADMIN caps
# (see the triagefactory service in docker-compose.yml). Running it in
# a container also keeps all netns/iptables/bundle churn off the host —
# everything dies with the container.
#
# The image is built with plain `docker build` (not compose) so the
# bench needs no .env / Postgres / GoTrue. The sandbox rootfs cache
# volume is shared with the compose stack when it exists, so a warm
# cache is reused; otherwise a dedicated volume is created and the
# first run pays the one-time alpine download + apk toolchain install
# (~5-10 min).
#
# Usage:
#   ./scripts/sandbox-bench.sh [sandbox-bench flags...]
#   ./scripts/sandbox-bench.sh -profile idle -levels 8,16,32 -hold 30s
#
# Results land in ./bench-results/ on the host.
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
echo "==> running bench (csv: bench-results/$CSV)"
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
    -csv "/bench-out/$CSV" "$@"

echo "==> done: bench-results/$CSV"
