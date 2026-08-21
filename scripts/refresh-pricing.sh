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
# Run daily by .github/workflows/refresh-pricing.yml. Three safety nets guard
# the gap between fast-moving upstream data and the frozen cost formula:
#   1. The in-package verification gate (go test ./internal/inference -run
#      TestPricing) — a moved anchor price, a broken cache multiplier, or
#      malformed JSON fails it loudly rather than misbilling quietly.
#   2. The schema-drift check below — a NEW per-token/cache cost field upstream
#      (a finer context tier, a new cache-TTL bucket) that computeTextCost
#      doesn't read is flagged so it becomes a reviewed formula change, not
#      silent underpricing in the data.
#   3. The pricing-eligibility gate below — a model is eligible for TF's model
#      picker iff it is mode=chat with tool support, has a >=64k input window,
#      carries both base prices (and a cache-read price if it claims caching),
#      and its litellm_provider maps to a bifrost provider in the TF-owned
#      internal/inference/provider_map.json (never written by this script).
#      Eligibility is a computed annotation — nothing is dropped from the
#      datasheet over it. The refresh REFUSES (no PR) when an eligible-gated
#      entry's provider has no map entry at all, so a new upstream provider is
#      a deliberate PR decision, never a silent gap; a provider already mapped
#      to "unsupported" is fine and silent.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

UPSTREAM_REPO="BerriAI/litellm"
UPSTREAM_FILE="model_prices_and_context_window.json"
DATASHEET="internal/inference/pricing_datasheet.json"
PROVENANCE="internal/inference/pricing_provenance.json"
PROVIDER_MAP="internal/inference/provider_map.json"

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
python3 - "$TMP_RAW" "$DATASHEET" "$PROVENANCE" "$PROVIDER_MAP" "$CANONICAL_URL" "$SHA" "$FETCHED" <<'PY'
import collections, json, sys

raw_path, datasheet_path, provenance_path, provider_map_path, url, sha, fetched = sys.argv[1:8]

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

# Pricing-eligibility gate. Eligibility is a computed ANNOTATION over the
# datasheet, never an exclusion from it — the datasheet above stays the broad,
# unfiltered-beyond-mode mirror it has always been. A model is eligible for
# TF's model picker iff every rule holds:
#   - mode == "chat" and supports_function_calling
#   - max_input_tokens >= 64000, read verbatim (no max_tokens fallback);
#     absent fails closed
#   - has both input_cost_per_token and output_cost_per_token
#   - if supports_prompt_caching, has cache_read_input_token_cost (a missing
#     cache-*write* price alone is fine — the automatic-caching-vendor
#     signature: OpenAI/Gemini/xAI/Azure charge nothing to populate a cache)
#   - litellm_provider maps to a real bifrost provider in provider_map.json
#
# "Gated" below is the first two rules only (mode+tools+window) — the
# smallest population that already represents a real, capable model. The
# refusal contract checks THAT population against provider_map.json, not the
# fully-eligible one: a capable model under a brand-new upstream provider
# label must force a mapping decision even if it also happens to lack a
# price today.
with open(provider_map_path) as f:
    provider_map = json.load(f)


def is_gated(entry):
    if entry.get("mode") != "chat" or not entry.get("supports_function_calling"):
        return False
    max_input = entry.get("max_input_tokens")
    return isinstance(max_input, (int, float)) and max_input >= 64000


def is_eligible(entry):
    if not is_gated(entry):
        return False
    if "input_cost_per_token" not in entry or "output_cost_per_token" not in entry:
        return False
    if entry.get("supports_prompt_caching") and "cache_read_input_token_cost" not in entry:
        return False
    provider = provider_map.get(entry.get("litellm_provider"))
    return provider is not None and provider != "unsupported"


unmapped = collections.OrderedDict()
for model, entry in out.items():
    if not is_gated(entry):
        continue
    provider = entry.get("litellm_provider")
    if provider not in provider_map:
        unmapped.setdefault(provider, []).append(model)

if unmapped:
    lines = [
        f"refusing to refresh: litellm_provider(s) with no entry in {provider_map_path}:",
    ]
    for provider in sorted(unmapped, key=lambda p: (p is None, p)):
        models = unmapped[provider]
        lines.append(f"  {provider!r}: {len(models)} eligible-gated model(s), e.g. {models[0]!r}")
    lines.append('Add a mapping (a bifrost provider constant, or "unsupported") before refreshing.')
    sys.exit("\n".join(lines))

eligible = sorted(
    (model, provider_map[entry["litellm_provider"]])
    for model, entry in out.items()
    if is_eligible(entry)
)

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

# Report, don't block: greppable markers the workflow turns into a "models
# shipped" section of the PR body — every entry eligible for TF's picker,
# under a provider TF already maps, in this snapshot.
by_provider = collections.Counter(provider for _, provider in eligible)
print(f"PRICING_ELIGIBLE_COUNT: {len(eligible)}")
print("PRICING_ELIGIBLE_BY_PROVIDER: " + ",".join(f"{p}={n}" for p, n in sorted(by_provider.items())))
print("PRICING_ELIGIBLE_MODELS_BEGIN")
for model, provider in eligible:
    print(f"{model}\t{provider}")
print("PRICING_ELIGIBLE_MODELS_END")
PY

echo "Done. Verify with: go test ./internal/inference -run TestPricing"
