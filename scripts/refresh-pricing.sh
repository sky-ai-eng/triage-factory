#!/usr/bin/env bash
# Refresh the vendored model-pricing snapshot the native-loop inference package
# prices from (internal/inference/pricing_datasheet.json + pricing_provenance.json).
#
# It re-fetches the authoritative upstream — LiteLLM's
# model_prices_and_context_window.json, the source getbifrost.ai/datasheet
# syncs — pins it to the exact commit it resolved, filters to the
# text-generation models the token cost formula prices (mode chat / responses /
# completion), and writes both data files. It writes ONLY data: it never edits
# Go source, so a refresh can change what a model costs, never how cost is
# computed. A bot can run it and open a data-only PR.
#
# Determinism: the output is key-sorted and indent=1 pretty-printed (matching
# the committed format), so an unchanged upstream produces no diff and a real
# price change shows up as a readable per-field line diff.
#
#   ./scripts/refresh-pricing.sh
#
# Run daily by .github/workflows/refresh-pricing.yml. Two safety nets guard the
# gap between fast-moving upstream data and the frozen cost formula:
#   1. The in-package verification gate (go test ./internal/inference -run
#      TestPricing) — a moved anchor price, a broken cache multiplier, or
#      malformed JSON fails it loudly rather than misbilling quietly.
#   2. The schema-drift check below — a NEW per-token/cache cost field upstream
#      (a finer context tier, a new cache-TTL bucket) that computeTextCost
#      doesn't read is flagged so it becomes a reviewed formula change, not
#      silent underpricing in the data.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

UPSTREAM_REPO="BerriAI/litellm"
UPSTREAM_FILE="model_prices_and_context_window.json"
DATASHEET="internal/inference/pricing_datasheet.json"
PROVENANCE="internal/inference/pricing_provenance.json"

command -v python3 >/dev/null 2>&1 || { echo "python3 not found on PATH" >&2; exit 1; }

# Resolve the upstream commit, then fetch the file AT that commit so the
# recorded provenance exactly matches the fetched bytes (no main-advances-
# mid-fetch race).
echo "Resolving $UPSTREAM_REPO@main ..."
SHA="$(git ls-remote "https://github.com/${UPSTREAM_REPO}.git" refs/heads/main | cut -f1)"
[ -n "$SHA" ] || { echo "could not resolve upstream main commit" >&2; exit 1; }
RAW_URL="https://raw.githubusercontent.com/${UPSTREAM_REPO}/${SHA}/${UPSTREAM_FILE}"
CANONICAL_URL="https://raw.githubusercontent.com/${UPSTREAM_REPO}/main/${UPSTREAM_FILE}"

TMP_RAW="$(mktemp)"
trap 'rm -f "$TMP_RAW"' EXIT
echo "Fetching $RAW_URL ..."
curl -fsSL "$RAW_URL" -o "$TMP_RAW"

FETCHED="$(date -u +%Y-%m-%d)"

echo "Filtering to text-generation models and writing $DATASHEET ..."
python3 - "$TMP_RAW" "$DATASHEET" "$PROVENANCE" "$CANONICAL_URL" "$SHA" "$FETCHED" <<'PY'
import collections, json, sys

raw_path, datasheet_path, provenance_path, url, sha, fetched = sys.argv[1:7]

with open(raw_path) as f:
    data = json.load(f)

TEXT_MODES = {"chat", "responses", "completion"}
out = collections.OrderedDict()
for key in sorted(data):
    entry = data[key]
    if isinstance(entry, dict) and entry.get("mode") in TEXT_MODES:
        out[key] = entry

if len(out) < 100:
    sys.exit(f"refusing to write a suspiciously small snapshot ({len(out)} models) — upstream may have changed shape")

# Schema-drift guard. These are the per-token/cache cost fields upstream is
# KNOWN to publish — the ones computeTextCost reads plus the tier/premium
# variants it deliberately does not model (priority/flex/batches/cache_hit and
# the 128k/256k/272k/512k context tiers). A field in one of these families that
# is NOT listed here is a NEW pricing dimension: computeTextCost would silently
# ignore it. When that happens, review whether the formula must model it, then
# add the field here to acknowledge it (modelled or intentionally skipped).
COST_FIELD_FAMILIES = (
    "input_cost_per_token",
    "output_cost_per_token",
    "cache_read_input_token_cost",
    "cache_creation_input_token_cost",
)
ACKNOWLEDGED_COST_FIELDS = {
    "cache_creation_input_token_cost",
    "cache_creation_input_token_cost_above_1hr",
    "cache_creation_input_token_cost_above_1hr_above_200k_tokens",
    "cache_creation_input_token_cost_above_200k_tokens",
    "cache_creation_input_token_cost_above_272k_tokens",
    "cache_creation_input_token_cost_flex",
    "cache_creation_input_token_cost_priority",
    "cache_read_input_token_cost",
    "cache_read_input_token_cost_above_200k_tokens",
    "cache_read_input_token_cost_above_200k_tokens_priority",
    "cache_read_input_token_cost_above_272k_tokens",
    "cache_read_input_token_cost_above_272k_tokens_priority",
    "cache_read_input_token_cost_above_512k_tokens",
    "cache_read_input_token_cost_flex",
    "cache_read_input_token_cost_priority",
    "input_cost_per_token",
    "input_cost_per_token_above_128k_tokens",
    "input_cost_per_token_above_200k_tokens",
    "input_cost_per_token_above_200k_tokens_priority",
    "input_cost_per_token_above_256k_tokens",
    "input_cost_per_token_above_272k_tokens",
    "input_cost_per_token_above_272k_tokens_priority",
    "input_cost_per_token_above_512k_tokens",
    "input_cost_per_token_batches",
    "input_cost_per_token_cache_hit",
    "input_cost_per_token_flex",
    "input_cost_per_token_priority",
    "output_cost_per_token",
    "output_cost_per_token_above_128k_tokens",
    "output_cost_per_token_above_200k_tokens",
    "output_cost_per_token_above_200k_tokens_priority",
    "output_cost_per_token_above_256k_tokens",
    "output_cost_per_token_above_272k_tokens",
    "output_cost_per_token_above_272k_tokens_priority",
    "output_cost_per_token_above_512k_tokens",
    "output_cost_per_token_batches",
    "output_cost_per_token_flex",
    "output_cost_per_token_priority",
}
seen = set()
for entry in out.values():
    for field in entry:
        if any(field == fam or field.startswith(fam) for fam in COST_FIELD_FAMILIES):
            seen.add(field)
drift = sorted(seen - ACKNOWLEDGED_COST_FIELDS)

with open(datasheet_path, "w") as f:
    json.dump(out, f, indent=1, sort_keys=True)
    f.write("\n")

provenance = collections.OrderedDict([
    ("source", url),
    ("commit", sha),
    ("fetched", fetched),
    ("filter", "mode in {chat, responses, completion}"),
])
with open(provenance_path, "w") as f:
    json.dump(provenance, f, indent=1)
    f.write("\n")

print(f"wrote {len(out)} models; upstream commit {sha}")
if drift:
    # A single-line, greppable marker the workflow surfaces in the PR. Not fatal
    # — prices still refresh; the formula review is the follow-up.
    print("PRICING_SCHEMA_DRIFT: " + ",".join(drift))
PY

echo "Done. Verify with: go test ./internal/inference -run TestPricing"
