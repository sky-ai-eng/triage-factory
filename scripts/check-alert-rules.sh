#!/usr/bin/env bash
#
# Checks the bundled Prometheus alerting rules with promtool from the stack's
# own Prometheus image, the version that evaluates them in a deployment:
#
#   promtool check rules   every file in docker/observability/rules/ parses
#   promtool test rules    the cases in scripts/alert-rules/ fire each alert
#                          on the series it is meant for
#
# Needs Docker. The compose-smoke workflow runs it in CI.
#
# Usage: ./scripts/check-alert-rules.sh

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

image=$(sed -nE 's/^[[:space:]]+image:[[:space:]]*(prom\/prometheus:[^[:space:]#]+).*/\1/p' docker-compose.yml)
if [ -z "$image" ]; then
  echo "check-alert-rules: no prom/prometheus image in docker-compose.yml" >&2
  exit 1
fi

promtool() {
  docker run --rm -v "$ROOT:/repo:ro" -w /repo --entrypoint promtool "$image" "$@"
}

promtool check rules docker/observability/rules/*.yml
promtool test rules scripts/alert-rules/*_test.yml
