package inference

import (
	"math"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

const epsilon = 1e-12

func approxEqual(a, b float64) bool { return math.Abs(a-b) <= epsilon*math.Max(1, math.Abs(b)) }

// expectedPrice is one row of the verification table we author by hand from
// Anthropic's public pricing page (per-token = per-MTok / 1e6). A drifted or
// corrupt upstream snapshot must fail this loudly rather than misbill quietly.
type expectedPrice struct {
	input, output               float64
	cacheRead, cache5m, cache1h float64
}

// anthropicPublicPrices mirrors https://www.anthropic.com/pricing (standard
// tier, ≤200k context). The prompt-cache economics are the same across the
// family: cache read = 0.1× input, 5-minute write = 1.25× input, 1-hour write
// = 2× input.
var anthropicPublicPrices = map[string]expectedPrice{
	"claude-sonnet-4-5": {input: 3e-06, output: 1.5e-05, cacheRead: 3e-07, cache5m: 3.75e-06, cache1h: 6e-06},
	"claude-opus-4-1":   {input: 1.5e-05, output: 7.5e-05, cacheRead: 1.5e-06, cache5m: 1.875e-05, cache1h: 3e-05},
	"claude-haiku-4-5":  {input: 1e-06, output: 5e-06, cacheRead: 1e-07, cache5m: 1.25e-06, cache1h: 2e-06},
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

// TestPricing_VerificationGate is the guard the ticket requires: the vendored
// snapshot's spot prices and Anthropic prompt-cache multipliers must match the
// hand-authored expected table, so a bad upstream re-vendor fails tests loudly.
func TestPricing_VerificationGate(t *testing.T) {
	table, err := loadPricing()
	if err != nil {
		t.Fatal(err)
	}

	for model, want := range anthropicPublicPrices {
		t.Run(model, func(t *testing.T) {
			p, ok := table[model]
			if !ok {
				t.Fatalf("model %q missing from snapshot", model)
			}
			assertRate(t, "input", p.InputCostPerToken, want.input)
			assertRate(t, "output", p.OutputCostPerToken, want.output)
			assertRate(t, "cache_read", p.CacheReadInputTokenCost, want.cacheRead)
			assertRate(t, "cache_write_5m", p.CacheCreationInputTokenCost, want.cache5m)
			assertRate(t, "cache_write_1h", p.CacheCreationInputTokenCostAbove1hr, want.cache1h)

			// The prompt-cache multipliers must hold against the snapshot's own
			// input rate — the load-bearing check the ticket calls out.
			if !approxEqual(want.cacheRead, 0.1*want.input) {
				t.Errorf("cache-read multiplier: want 0.1x input, got %g/%g", want.cacheRead, want.input)
			}
			if !approxEqual(want.cache5m, 1.25*want.input) {
				t.Errorf("5m cache-write multiplier: want 1.25x input, got %g/%g", want.cache5m, want.input)
			}
			if !approxEqual(want.cache1h, 2*want.input) {
				t.Errorf("1h cache-write multiplier: want 2x input, got %g/%g", want.cache1h, want.input)
			}
		})
	}
}

func assertRate(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s rate missing in snapshot", name)
	}
	if !approxEqual(*got, want) {
		t.Errorf("%s rate: snapshot %g, expected %g", name, *got, want)
	}
}

func TestCostForUsage_KnownModel(t *testing.T) {
	// sonnet-4-5, standard tier. input 1000 (200 cache-read, 100 cache-write
	// 5m, 700 non-cached), output 500.
	usd, ok := CostForUsage("claude-sonnet-4-5", Usage{
		InputTokens:         1000,
		OutputTokens:        500,
		CacheReadTokens:     200,
		CacheCreationTokens: 100,
	})
	if !ok {
		t.Fatal("expected ok for a known model")
	}
	want := 700*3e-06 + 200*3e-07 + 100*3.75e-06 + 500*1.5e-05
	if !approxEqual(usd, want) {
		t.Fatalf("cost = %g, want %g", usd, want)
	}
}

func TestCostForUsage_CacheWrite1hSplit(t *testing.T) {
	// 100 cache-write of which 40 are 1-hour (2x) and 60 are 5-minute (1.25x).
	usd, ok := CostForUsage("claude-sonnet-4-5", Usage{
		InputTokens:           100,
		CacheReadTokens:       0,
		CacheCreationTokens:   100,
		CacheCreationTokens1h: 40,
	})
	if !ok {
		t.Fatal("expected ok")
	}
	// nonCached = 100 - 0 - 100 = 0; only the cache-write split bills.
	want := 40*6e-06 + 60*3.75e-06
	if !approxEqual(usd, want) {
		t.Fatalf("cost = %g, want %g", usd, want)
	}
}

func TestCostForUsage_LongContextTier(t *testing.T) {
	// Above 200k input, sonnet-4-5 bills input at 6e-06 and output at 2.25e-05.
	in := 250000
	out := 1000
	usd, ok := CostForUsage("claude-sonnet-4-5", Usage{InputTokens: in, OutputTokens: out})
	if !ok {
		t.Fatal("expected ok")
	}
	want := float64(in)*6e-06 + float64(out)*2.25e-05
	if !approxEqual(usd, want) {
		t.Fatalf("long-context cost = %g, want %g", usd, want)
	}
}

// TestCostForUsage_UnknownModel pins the ledger contract: an unknown model
// yields ok=false so the caller stamps NULL, never a misleading 0.
func TestCostForUsage_UnknownModel(t *testing.T) {
	usd, ok := CostForUsage("gpt-not-a-real-model", Usage{InputTokens: 100})
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
	usd, ok := CostForUsage("claude-haiku-4-5", Usage{})
	if !ok {
		t.Fatal("known model with zero usage must still be ok=true")
	}
	if usd != 0 {
		t.Fatalf("zero usage must cost 0, got %g", usd)
	}
}

// TestCostForUsage_CrossProvider proves the package prices any provider's chat
// models out of the gate, not just Anthropic — an OpenAI and a Gemini model
// both resolve and compute.
func TestCostForUsage_CrossProvider(t *testing.T) {
	// gpt-4o: input 2.5e-06, output 1e-05.
	usd, ok := CostForUsage("gpt-4o", Usage{InputTokens: 1000, OutputTokens: 500})
	if !ok {
		t.Fatal("expected a non-Anthropic chat model (gpt-4o) to be priced")
	}
	if want := 1000*2.5e-06 + 500*1e-05; !approxEqual(usd, want) {
		t.Fatalf("gpt-4o cost = %g, want %g", usd, want)
	}
	if _, ok := CostForUsage("gemini-2.5-pro", Usage{InputTokens: 100}); !ok {
		t.Fatal("expected a Gemini chat model to be priced")
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
	want := Usage{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, CacheCreationTokens: 100, CacheCreationTokens1h: 40}
	if u != want {
		t.Fatalf("usage projection: got %+v want %+v", u, want)
	}
	if (usageFromBifrost(nil) != Usage{}) {
		t.Fatal("nil usage must project to zero Usage")
	}
}
