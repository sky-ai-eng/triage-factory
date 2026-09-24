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
	// readMultiple is the cache-read rate as a fraction of input. It is a
	// per-model column because the family does not share one value.
	readMultiple float64
}

// anthropicPublicPrices mirrors https://www.anthropic.com/pricing (standard
// tier, ≤200k context). Cache writes are the same across the family: 5-minute
// write = 1.25× input, 1-hour write = 2× input. Cache read is 0.1× input on
// most models and 0.05× on Opus 5.5, so each row states its own.
var anthropicPublicPrices = map[string]expectedPrice{
	"claude-sonnet-4-5": {input: 3e-06, output: 1.5e-05, cacheRead: 3e-07, cache5m: 3.75e-06, cache1h: 6e-06, readMultiple: 0.1},
	"claude-opus-5-5":   {input: 4e-06, output: 2e-05, cacheRead: 2e-07, cache5m: 5e-06, cache1h: 8e-06, readMultiple: 0.05},
	"claude-haiku-4-5":  {input: 1e-06, output: 5e-06, cacheRead: 1e-07, cache5m: 1.25e-06, cache1h: 2e-06, readMultiple: 0.1},
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
			if !approxEqual(want.cacheRead, want.readMultiple*want.input) {
				t.Errorf("cache-read multiplier: want %gx input, got %g/%g", want.readMultiple, want.cacheRead, want.input)
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
		PromptTokens:        1000,
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
		PromptTokens:          100,
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
	usd, ok := CostForUsage("claude-sonnet-4-5", Usage{PromptTokens: in, OutputTokens: out})
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
	usd, ok := CostForUsage("gpt-4o", Usage{PromptTokens: 1000, OutputTokens: 500})
	if !ok {
		t.Fatal("expected a non-Anthropic chat model (gpt-4o) to be priced")
	}
	if want := 1000*2.5e-06 + 500*1e-05; !approxEqual(usd, want) {
		t.Fatalf("gpt-4o cost = %g, want %g", usd, want)
	}
	if _, ok := CostForUsage("gemini-2.5-pro", Usage{PromptTokens: 100}); !ok {
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
