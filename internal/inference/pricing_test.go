package inference

import (
	"maps"
	"math"
	"slices"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

const epsilon = 1e-12

func approxEqual(a, b float64) bool { return math.Abs(a-b) <= epsilon*math.Max(1, math.Abs(b)) }

// pricedModel returns a snapshot key whose row carries an input and an output
// rate, under the given litellm_provider label (any label when empty): the
// first such key in sorted order, so the choice is stable for a given
// snapshot. A test that needs a real model takes one this way rather than
// naming it, because which models the snapshot carries is upstream's data and
// a refresh may retire any of them. Prices for the models TF offers are
// checked by modelcatalog's TestPricing_CatalogRates.
func pricedModel(t *testing.T, provider string) string {
	t.Helper()
	table, err := loadPricing()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range slices.Sorted(maps.Keys(table)) {
		p := table[k]
		if (provider == "" || p.LiteLLMProvider == provider) && p.InputCostPerToken != nil && p.OutputCostPerToken != nil {
			return k
		}
	}
	t.Fatalf("snapshot has no priced row for provider %q", provider)
	return ""
}

func TestPricingProvenance(t *testing.T) {
	source, commit, fetched := PricingProvenance()
	if source == "" || commit == "" || fetched == "" {
		t.Fatalf("provenance must be populated from pricing_provenance.json: source=%q commit=%q fetched=%q", source, commit, fetched)
	}
}

func TestPricing_DatasheetLoads(t *testing.T) {
	if err := PricingLoadError(); err != nil {
		t.Fatalf("embedded datasheet must parse: %v", err)
	}
	table, err := loadPricing()
	if err != nil {
		t.Fatal(err)
	}
	if len(table) == 0 {
		t.Fatal("datasheet is empty")
	}
}

func TestComputeTextCost_CacheWrite1hSplit(t *testing.T) {
	p := modelPrice{
		InputCostPerToken:                   f64(2e-06),
		CacheCreationInputTokenCost:         f64(2.5e-06),
		CacheCreationInputTokenCostAbove1hr: f64(4e-06),
	}
	// 100 cache-write of which 40 are 1-hour and 60 are 5-minute.
	usd := computeTextCost(p, Usage{
		PromptTokens:          100,
		CacheCreationTokens:   100,
		CacheCreationTokens1h: 40,
	})
	// nonCached = 100 - 0 - 100 = 0; only the cache-write split bills.
	want := 40*4e-06 + 60*2.5e-06
	if !approxEqual(usd, want) {
		t.Fatalf("cost = %g, want %g", usd, want)
	}
}

// TestComputeTextCost_LongContextTier pins the formula's tier selection against
// a synthetic price rather than a snapshot row. Which models publish a >200k
// tier is upstream's data and a refresh may legitimately remove one; how the
// formula applies a tier is this package's, and must not depend on some model
// still carrying one.
func TestComputeTextCost_LongContextTier(t *testing.T) {
	p := modelPrice{
		InputCostPerToken:                           f64(3e-06),
		InputCostPerTokenAbove200k:                  f64(6e-06),
		OutputCostPerToken:                          f64(1.5e-05),
		OutputCostPerTokenAbove200k:                 f64(2.25e-05),
		CacheReadInputTokenCost:                     f64(3e-07),
		CacheReadInputTokenCostAbove200:             f64(6e-07),
		CacheCreationInputTokenCostAbove1hr:         f64(6e-06),
		CacheCreationInputTokenCostAbove1hrAbove200: f64(1.2e-05),
		// No tiered 5-minute write rate: that bucket bills at its base rate on
		// both sides of the boundary.
		CacheCreationInputTokenCost: f64(3.75e-06),
	}

	// 250k prompt: 200k non-cached, 30k cache read, 20k cache write of which
	// 5k are 1-hour.
	above := Usage{PromptTokens: 250000, OutputTokens: 1000, CacheReadTokens: 30000, CacheCreationTokens: 20000, CacheCreationTokens1h: 5000}
	want := 200000*6e-06 + 30000*6e-07 + 5000*1.2e-05 + 15000*3.75e-06 + 1000*2.25e-05
	if got := computeTextCost(p, above); !approxEqual(got, want) {
		t.Errorf("above 200k: cost = %g, want %g", got, want)
	}

	// The boundary itself is not above it.
	at := Usage{PromptTokens: 200000, OutputTokens: 1000}
	want = 200000*3e-06 + 1000*1.5e-05
	if got := computeTextCost(p, at); !approxEqual(got, want) {
		t.Errorf("at 200k: cost = %g, want %g", got, want)
	}
}

// TestCostForUsage_UnknownModel pins the ledger contract: an unknown model
// yields ok=false so the caller stamps NULL, never a misleading 0.
func TestCostForUsage_UnknownModel(t *testing.T) {
	usd, ok := CostForUsage("gpt-not-a-real-model", Usage{PromptTokens: 100})
	if ok {
		t.Fatalf("unknown model must return ok=false, got usd=%g", usd)
	}
	if usd != 0 {
		t.Fatalf("unknown model must return 0 usd alongside ok=false, got %g", usd)
	}
}

func TestCostForUsage_FreeIsNotUnknown(t *testing.T) {
	// A genuinely-zero usage on a known model is (0, true) — distinct from an
	// unknown model's (0, false).
	usd, ok := CostForUsage(pricedModel(t, ""), Usage{})
	if !ok {
		t.Fatal("known model with zero usage must still be ok=true")
	}
	if usd != 0 {
		t.Fatalf("zero usage must cost 0, got %g", usd)
	}
}

// TestCostForUsage_CrossProvider proves the package prices any provider's chat
// models out of the gate, not just Anthropic — an OpenAI and a Gemini row both
// resolve and compute from their own rates.
func TestCostForUsage_CrossProvider(t *testing.T) {
	table, err := loadPricing()
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"openai", "gemini"} {
		model := pricedModel(t, provider)
		usd, ok := CostForUsage(model, Usage{PromptTokens: 1000, OutputTokens: 500})
		if !ok {
			t.Errorf("%s row %q is not priced", provider, model)
			continue
		}
		p := table[model]
		if want := 1000**p.InputCostPerToken + 500**p.OutputCostPerToken; !approxEqual(usd, want) {
			t.Errorf("%s cost = %g, want %g", model, usd, want)
		}
	}
}

// TestCostForUsage_NonTextModeUnpriced pins the mode guard: a model whose
// modality the text formula can't price returns ok=false, never a wrong number.
// Exercised through a synthetic table since the snapshot is pre-filtered to
// text models.
func TestCostForUsage_NonTextModeUnpriced(t *testing.T) {
	for mode, priceable := range map[string]bool{
		"chat": true, "responses": true, "completion": true, "": true,
		"embedding": false, "image_generation": false, "audio_speech": false, "rerank": false,
	} {
		if got := isPriceableMode(mode); got != priceable {
			t.Errorf("isPriceableMode(%q) = %v, want %v", mode, got, priceable)
		}
	}
}

func TestLookupPrice_BedrockRegionPrefixStrip(t *testing.T) {
	table := map[string]modelPrice{
		"anthropic.claude-x": {InputCostPerToken: f64(1e-06)},
	}
	for _, model := range []string{
		"us.anthropic.claude-x",
		"eu.anthropic.claude-x",
		"apac.anthropic.claude-x",
		"global.anthropic.claude-x",
	} {
		if _, ok := lookupPrice(table, model); !ok {
			t.Errorf("region-prefixed %q should resolve to its base row", model)
		}
	}
	if _, ok := lookupPrice(table, "unknown.model"); ok {
		t.Error("an unrelated model must not resolve")
	}
}

func TestUsageFromBifrost(t *testing.T) {
	u := usageFromBifrost(&schemas.BifrostLLMUsage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		PromptTokensDetails: &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  200,
			CachedWriteTokens: 100,
			CachedWriteTokenDetails: &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens1h: 40,
			},
		},
	})
	want := Usage{PromptTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, CacheCreationTokens: 100, CacheCreationTokens1h: 40}
	if u != want {
		t.Fatalf("usage projection: got %+v want %+v", u, want)
	}
	if (usageFromBifrost(nil) != Usage{}) {
		t.Fatal("nil usage must project to zero Usage")
	}
}

// TestModelWindow pins the lookup rules compaction depends on. Expected windows
// are read from the snapshot itself, because the numbers are upstream's data and
// change on the refresh cadence; what this package owns is how a model id
// resolves to an entry.
func TestModelWindow(t *testing.T) {
	table, err := loadPricing()
	if err != nil {
		t.Fatal(err)
	}
	snapshotWindow := func(key string) int {
		t.Helper()
		p, ok := table[key]
		if !ok || p.MaxInputTokens == nil || *p.MaxInputTokens <= 0 {
			t.Fatalf("snapshot entry %q must exist and carry max_input_tokens", key)
		}
		return int(*p.MaxInputTokens)
	}

	const base = "claude-haiku-4-5"
	want := snapshotWindow(base)

	// A known model returns its datasheet window.
	if got, ok := ModelWindow(base); !ok || got != want {
		t.Errorf("ModelWindow(%q) = (%d, %v), want (%d, true)", base, got, ok, want)
	}

	// A region-prefixed id absent from the table resolves to its unprefixed
	// entry, the same rule as pricing lookup.
	prefixed := "global." + base
	if _, ok := table[prefixed]; ok {
		t.Fatalf("%q must be absent from the snapshot for this case to exercise the prefix strip", prefixed)
	}
	if got, ok := ModelWindow(prefixed); !ok || got != want {
		t.Errorf("ModelWindow(%q) = (%d, %v), want (%d, true)", prefixed, got, ok, want)
	}

	// An unknown model is not found: the caller must not guess a window.
	if got, ok := ModelWindow("some-model-nobody-heard-of"); ok || got != 0 {
		t.Errorf("unknown model = (%d, %v), want (0, false)", got, ok)
	}

	// An entry with no max_input_tokens is not found either.
	var noWindow string
	for k, p := range table {
		if p.MaxInputTokens == nil {
			noWindow = k
			break
		}
	}
	if noWindow == "" {
		t.Skip("snapshot has no entry without max_input_tokens")
	}
	if got, ok := ModelWindow(noWindow); ok || got != 0 {
		t.Errorf("ModelWindow(%q) with no window = (%d, %v), want (0, false)", noWindow, got, ok)
	}
}

// TestUsage_NonCachedInputTokens pins the projection between the two token
// conventions this package sits between: providers are normalized into one
// prompt count that includes its cache buckets (what pricing needs), while
// every stored ledger column is disjoint (what every reader of them sums).
func TestUsage_NonCachedInputTokens(t *testing.T) {
	u := Usage{PromptTokens: 100_000, OutputTokens: 500, CacheReadTokens: 90_000, CacheCreationTokens: 5_000}
	if got := u.NonCachedInputTokens(); got != 5_000 {
		t.Errorf("NonCachedInputTokens = %d, want 5000", got)
	}
	if sum := u.NonCachedInputTokens() + u.CacheReadTokens + u.CacheCreationTokens; sum != u.PromptTokens {
		t.Errorf("the three prompt buckets sum to %d, want the prompt's %d exactly once", sum, u.PromptTokens)
	}

	if got := (Usage{PromptTokens: 1_000}).NonCachedInputTokens(); got != 1_000 {
		t.Errorf("an uncached prompt = %d, want the whole prompt", got)
	}

	// A payload whose parts exceed its total is clamped the same way
	// computeTextCost clamps it, so the row and the price it was charged never
	// describe two different requests.
	if got := (Usage{PromptTokens: 10, CacheReadTokens: 400}).NonCachedInputTokens(); got != 0 {
		t.Errorf("NonCachedInputTokens = %d, want 0 rather than a negative count", got)
	}
}
