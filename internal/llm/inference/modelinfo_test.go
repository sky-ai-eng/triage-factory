package inference

import (
	"strings"
	"testing"
)

// The join the model catalog performs, at the level of one row: what the
// snapshot says about a model must be the same thing the cost formula would
// charge it at.
func TestModelInfo_ReadsTheSameRowPricingDoes(t *testing.T) {
	const model = "claude-sonnet-5"
	info, ok := ModelInfo(model)
	if !ok {
		t.Fatalf("ModelInfo(%q) not found in the committed snapshot", model)
	}
	if info.Provider != "anthropic" {
		t.Errorf("provider = %q, want %q", info.Provider, "anthropic")
	}
	if !info.SupportsPromptCaching {
		t.Error("supports_prompt_caching = false, want true")
	}

	window, ok := ModelWindow(model)
	if !ok {
		t.Fatalf("ModelWindow(%q) not found", model)
	}
	if info.MaxInputTokens != window {
		t.Errorf("MaxInputTokens = %d, want %d (ModelWindow's answer)", info.MaxInputTokens, window)
	}

	// One million output tokens priced through the cost formula must cost
	// exactly a million times the rate ModelInfo reports, or a catalog built
	// from these rates would quote a price the ledger does not charge.
	cost, ok := CostForUsage(model, Usage{OutputTokens: 1_000_000})
	if !ok {
		t.Fatalf("CostForUsage(%q) could not price the model", model)
	}
	if want := info.OutputCostPerToken * 1_000_000; cost != want {
		t.Errorf("cost = %v, want %v (ModelInfo's output rate)", cost, want)
	}
}

func TestModelInfo_UnknownKey(t *testing.T) {
	if _, ok := ModelInfo("vendor-model-that-does-not-exist"); ok {
		t.Error("ModelInfo resolved a key the snapshot does not carry")
	}
}

// A row upstream publishes no price for cannot be offered: reporting it would
// put a model in a picker at $0, since a nil rate bills as zero.
func TestModelInfo_RefusesAnUnpricedRow(t *testing.T) {
	table, err := loadPricing()
	if err != nil {
		t.Fatalf("load datasheet: %v", err)
	}
	var unpriced string
	for key, p := range table {
		if p.InputCostPerToken == nil || p.OutputCostPerToken == nil {
			unpriced = key
			break
		}
	}
	if unpriced == "" {
		t.Skip("the committed snapshot carries no unpriced row to exercise")
	}
	if _, ok := ModelInfo(unpriced); ok {
		t.Errorf("ModelInfo(%q) reported an unpriced row as usable", unpriced)
	}
}

// Bedrock inference-profile ids are region-prefixed spellings of a model. Where
// upstream carries no row of its own for the prefixed id, describing one must
// fall back to the bare key exactly as billing does — otherwise a catalog entry
// naming a profile id would quote prices from nowhere. (Where upstream DOES
// carry the prefixed row, it holds that region's own multiplied rates and the
// exact match is the right answer; this covers the fallback, not that case.)
func TestModelInfo_StripsTheBedrockRegionPrefixLikePricingDoes(t *testing.T) {
	table, err := loadPricing()
	if err != nil {
		t.Fatalf("load datasheet: %v", err)
	}
	var base string
	for key, p := range table {
		if p.InputCostPerToken == nil || p.OutputCostPerToken == nil {
			continue
		}
		if !strings.HasPrefix(key, "anthropic.") {
			continue
		}
		if _, prefixed := table["us."+key]; prefixed {
			continue
		}
		base = key
		break
	}
	if base == "" {
		t.Skip("every bedrock-shaped row in the committed snapshot carries its own region-prefixed entry")
	}
	direct, ok := ModelInfo(base)
	if !ok {
		t.Fatalf("ModelInfo(%q) not found", base)
	}
	profile, ok := ModelInfo("us." + base)
	if !ok {
		t.Fatalf("ModelInfo(%q) did not fall back to the base key", "us."+base)
	}
	if direct != profile {
		t.Errorf("profile id resolved a different row:\n got %+v\nwant %+v", profile, direct)
	}
}
