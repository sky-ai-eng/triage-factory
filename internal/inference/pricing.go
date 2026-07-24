package inference

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"
)

// pricing_datasheet.json is a snapshot of upstream model PRICES — the per-model
// rate numbers only. It, and pricing_provenance.json beside it, are the sole
// outputs of scripts/refresh-pricing.sh (run daily by the refresh-pricing
// workflow): a pure data operation that rewrites these two JSON files and never
// touches Go source. A refresh can change what a model costs — never how cost is
// computed (that is the formula in computeTextCost, which only a human edits).
//
// Source: LiteLLM's model_prices_and_context_window.json (the table
// getbifrost.ai/datasheet syncs), filtered to the text-generation models the
// formula prices (mode chat/responses/completion) across every provider — not
// just the ones TF drives today, so the package prices any chat model out of the
// gate. Non-text modalities (image/audio/embedding/rerank/…) are dropped;
// CostForUsage returns ok=false for a mode it doesn't price. Never a TF-authored
// price table. pricing_provenance.json records the exact upstream commit + date,
// kept auditable at runtime via PricingProvenance.

//go:embed pricing_datasheet.json
var pricingDatasheetJSON []byte

//go:embed pricing_provenance.json
var pricingProvenanceJSON []byte

// PricingProvenance reports where the embedded datasheet snapshot came from so
// the source is auditable at runtime. Values live in pricing_provenance.json so
// a refresh touches only data, never this file.
func PricingProvenance() (source, commit, fetched string) {
	var p struct {
		Source  string `json:"source"`
		Commit  string `json:"commit"`
		Fetched string `json:"fetched"`
	}
	_ = json.Unmarshal(pricingProvenanceJSON, &p)
	return p.Source, p.Commit, p.Fetched
}

// tokenTierAbove200K is the long-context boundary: above it a model bills the
// per-token/-cache rates at their *_above_200k tier (Anthropic's 1M-context
// tier; a model with no such rate falls back to its base rate). Mirrors
// bifrost's TokenTierAbove200K. The vendored calc reproduces the standard
// on-demand text path; premium tiers (fast/flex/priority) are intentionally not
// modeled — the native loop never requests them.
const tokenTierAbove200K = 200000

// modelPrice is the subset of the datasheet's per-model pricing the text-cost
// path reads. Field names/json tags mirror the upstream datasheet (LiteLLM /
// bifrost TableModelPricing) so the snapshot unmarshals directly; unread fields
// (audio, image, video, search, supports_* flags, the fast/flex/priority tiers)
// are ignored on decode.
type modelPrice struct {
	// Mode is the model's modality ("chat" | "responses" | "completion" for the
	// text models this snapshot carries). CostForUsage prices only these — a
	// non-text mode returns ok=false rather than a wrong number from the text
	// formula.
	Mode string `json:"mode"`

	InputCostPerToken  *float64 `json:"input_cost_per_token"`
	OutputCostPerToken *float64 `json:"output_cost_per_token"`

	InputCostPerTokenAbove200k  *float64 `json:"input_cost_per_token_above_200k_tokens"`
	OutputCostPerTokenAbove200k *float64 `json:"output_cost_per_token_above_200k_tokens"`

	CacheReadInputTokenCost         *float64 `json:"cache_read_input_token_cost"`
	CacheReadInputTokenCostAbove200 *float64 `json:"cache_read_input_token_cost_above_200k_tokens"`

	CacheCreationInputTokenCost         *float64 `json:"cache_creation_input_token_cost"`
	CacheCreationInputTokenCostAbove200 *float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`

	CacheCreationInputTokenCostAbove1hr         *float64 `json:"cache_creation_input_token_cost_above_1hr"`
	CacheCreationInputTokenCostAbove1hrAbove200 *float64 `json:"cache_creation_input_token_cost_above_1hr_above_200k_tokens"`
}

var (
	pricingOnce  sync.Once
	pricingTable map[string]modelPrice
	pricingErr   error
)

// loadPricing parses the embedded datasheet once. A parse failure is fatal to
// pricing (returned as ok=false with the error surfaced via PricingLoadError)
// rather than silently misbilling.
func loadPricing() (map[string]modelPrice, error) {
	pricingOnce.Do(func() {
		var table map[string]modelPrice
		if err := json.Unmarshal(pricingDatasheetJSON, &table); err != nil {
			pricingErr = fmt.Errorf("inference: parse pricing datasheet: %w", err)
			return
		}
		pricingTable = table
	})
	return pricingTable, pricingErr
}

// PricingLoadError reports whether the embedded datasheet failed to parse. It
// exists so a caller (or a startup check) can fail loudly on a corrupt
// snapshot instead of discovering it as a stream of ok=false stamps.
func PricingLoadError() error {
	_, err := loadPricing()
	return err
}

// Usage is the neutral token accounting CostForUsage prices from — the same
// four columns messages stores plus the 1-hour cache-write split Anthropic
// bills at a higher rate. Built from a bifrost stream via usageFromBifrost.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	// CacheCreationTokens1h is the subset of CacheCreationTokens written at the
	// 1-hour TTL (billed at 2× vs the 5-minute 1.25×). Zero when unknown or all
	// writes are 5-minute.
	CacheCreationTokens1h int
}

// CostForUsage prices one message's usage in USD from the pinned datasheet.
// It prices any text model (any provider) the snapshot carries; ok is false for
// a model the snapshot doesn't carry or one whose mode the text formula can't
// price — callers stamp NULL, never 0 (0 means genuinely free in the ledger).
// Lookup is exact on the model id, with a Bedrock region-prefix
// (us./eu./apac./global.) strip as a fallback.
func CostForUsage(model string, usage Usage) (float64, bool) {
	table, err := loadPricing()
	if err != nil || table == nil {
		return 0, false
	}
	price, ok := lookupPrice(table, model)
	if !ok || !isPriceableMode(price.Mode) {
		return 0, false
	}
	return computeTextCost(price, usage), true
}

// isPriceableMode reports whether a model's modality is one the text-cost path
// prices. The snapshot is pre-filtered to these, so this is defense in depth: a
// future re-vendor that widened the filter can't silently mis-price an
// image/audio/embedding model through the token formula. An empty mode (a rare
// untagged datasheet entry) is treated as priceable.
func isPriceableMode(mode string) bool {
	switch mode {
	case "", "chat", "responses", "completion":
		return true
	default:
		return false
	}
}

// lookupPrice resolves a model id to its price entry: exact match first, then a
// single Bedrock region-prefix strip so an inference-profile id like
// "us.anthropic.claude-..." finds the "anthropic.claude-..." row.
func lookupPrice(table map[string]modelPrice, model string) (modelPrice, bool) {
	if p, ok := table[model]; ok {
		return p, true
	}
	for _, pfx := range []string{"us.", "eu.", "apac.", "global."} {
		if strings.HasPrefix(model, pfx) {
			if p, ok := table[strings.TrimPrefix(model, pfx)]; ok {
				return p, true
			}
		}
	}
	return modelPrice{}, false
}

// computeTextCost is the pricing FORMULA — the one part of this package the
// daily data refresh never touches. It is vendored from bifrost
// framework/modelcatalog computeTextCost (fork commit
// 0f07e6f726be1bd46e81c0982ff0618435560be9) and changes only by a deliberate
// human edit. It reproduces bifrost's standard-tier text billing: non-cached
// prompt tokens at the input rate, cache reads at the cache-read rate, cache
// writes split into 1-hour (2×) and 5-minute (1.25×) buckets, and completion
// tokens at the output rate — each rate selected by the >200k long-context
// tier. The clamp order (reads ≤ prompt, writes ≤ prompt−reads, 1h ≤ writes)
// matches upstream so a malformed usage payload can't bill negative.
//
// The risk this comment guards against: the datasheet's SHAPE can outrun a
// frozen formula. A new upstream cost dimension this doesn't read (a finer
// context tier, a new cache-TTL bucket) would silently underprice the models
// that use it. That is why scripts/refresh-pricing.sh flags any per-token/cache
// cost field it doesn't recognize — so a new dimension surfaces as a reviewed
// change to this formula, not silent drift in the data.
func computeTextCost(p modelPrice, u Usage) float64 {
	prompt := u.InputTokens
	completion := u.OutputTokens
	cachedRead := u.CacheReadTokens
	cachedWrite := u.CacheCreationTokens
	cachedWrite1h := u.CacheCreationTokens1h

	if cachedRead > prompt {
		cachedRead = prompt
	}
	if cachedWrite > prompt-cachedRead {
		cachedWrite = prompt - cachedRead
	}
	if cachedWrite1h > cachedWrite {
		cachedWrite1h = cachedWrite
	}

	above := prompt > tokenTierAbove200K
	inputRate := tieredRate(p.InputCostPerToken, p.InputCostPerTokenAbove200k, above)
	outputRate := tieredRate(p.OutputCostPerToken, p.OutputCostPerTokenAbove200k, above)
	cacheReadRate := tieredRate(p.CacheReadInputTokenCost, p.CacheReadInputTokenCostAbove200, above)
	cacheWriteRate := tieredRate(p.CacheCreationInputTokenCost, p.CacheCreationInputTokenCostAbove200, above)
	cacheWrite1hRate := tieredRate(p.CacheCreationInputTokenCostAbove1hr, p.CacheCreationInputTokenCostAbove1hrAbove200, above)

	nonCached := prompt - cachedRead - cachedWrite

	cost := float64(nonCached) * inputRate
	cost += float64(cachedRead) * cacheReadRate
	if cachedWrite1h > 0 {
		cost += float64(cachedWrite1h) * cacheWrite1hRate
		cost += float64(cachedWrite-cachedWrite1h) * cacheWriteRate
	} else {
		cost += float64(cachedWrite) * cacheWriteRate
	}
	cost += float64(completion) * outputRate
	return cost
}

// tieredRate returns the >200k rate when the request is over the long-context
// boundary and that rate is set, else the base rate; a nil base falls back to 0
// (an unpriced token category, matching bifrost's nil-pointer semantics).
func tieredRate(base, above200k *float64, above bool) float64 {
	if above && above200k != nil {
		return *above200k
	}
	if base != nil {
		return *base
	}
	// A cache rate with no explicit entry falls back to the input rate upstream;
	// for the categories TF prices (which always carry explicit cache rates on
	// Anthropic models) this fallback is never hit, so 0 is the safe "unpriced"
	// value rather than a silent misattribution.
	return 0
}

// usageFromBifrost projects a bifrost usage struct onto the neutral Usage,
// pulling cache read/write counts (and the 1-hour write split) out of the
// prompt-token details. A nil usage yields the zero Usage.
func usageFromBifrost(u *schemas.BifrostLLMUsage) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if d := u.PromptTokensDetails; d != nil {
		out.CacheReadTokens = d.CachedReadTokens
		out.CacheCreationTokens = d.CachedWriteTokens
		if d.CachedWriteTokenDetails != nil {
			out.CacheCreationTokens1h = d.CachedWriteTokenDetails.CachedWriteTokens1h
		}
	}
	return out
}
