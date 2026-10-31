package modelcatalog

import (
	"math"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
)

// TestPricing_CatalogRates is the pricing refresh's verification gate for the
// models TF offers: the refresh workflow runs it before it opens a PR, and a
// failure stops the refresh. It checks each row for completeness and internal
// consistency, never for a specific price. A price upstream moves is the
// refresh doing its job, and is reviewed in the PR's diff; a row TF cannot bill
// correctly from is what must not ship, because the team daily cost cap is
// enforced against spend priced from these rates.
func TestPricing_CatalogRates(t *testing.T) {
	if err := JoinError(); err != nil {
		t.Fatalf("a supported model no longer joins the snapshot: %v", err)
	}
	for _, e := range Entries() {
		t.Run(e.Key, func(t *testing.T) {
			info, ok := inference.ModelInfo(e.Key)
			if !ok {
				t.Fatal("no priced row in the snapshot")
			}

			// Every rate a request bills on. TF marks cache breakpoints on
			// both providers it drives, so every model here bills cache
			// writes: a missing write rate is a field upstream renamed,
			// billing each write at $0, not a vendor that caches for free.
			for name, rate := range map[string]float64{
				"input":       info.InputCostPerToken,
				"output":      info.OutputCostPerToken,
				"cache read":  info.CacheReadCostPerToken,
				"cache write": info.CacheWriteCostPerToken,
			} {
				if rate <= 0 {
					t.Errorf("%s rate %g, want a positive rate", name, rate)
				}
			}

			// Claude prices a 5-minute cache write at 1.25× input on every
			// model and route, so a row that breaks it has a rate copied
			// wrong. A non-Claude model joining the catalog needs its own rule
			// here.
			if !approxEqual(info.CacheWriteCostPerToken, 1.25*info.InputCostPerToken) {
				t.Errorf("cache write %g is not 1.25× input %g", info.CacheWriteCostPerToken, info.InputCostPerToken)
			}
			if info.CacheReadCostPerToken >= info.InputCostPerToken {
				t.Errorf("cache read %g is not cheaper than input %g", info.CacheReadCostPerToken, info.InputCostPerToken)
			}

			if _, ok := inference.ModelMaxOutput(e.Key); !ok {
				t.Error("no max_output_tokens: every call would fall back to the unknown-model budget")
			}

			// The ledger charges the rates the catalog displays. 1000 prompt
			// tokens: 700 non-cached, 200 cache read, 100 cache write.
			usd, ok := inference.CostForUsage(e.Key, inference.Usage{
				PromptTokens:        1000,
				OutputTokens:        500,
				CacheReadTokens:     200,
				CacheCreationTokens: 100,
			})
			if !ok {
				t.Fatal("CostForUsage cannot price a model the catalog offers")
			}
			want := 700*info.InputCostPerToken + 200*info.CacheReadCostPerToken +
				100*info.CacheWriteCostPerToken + 500*info.OutputCostPerToken
			if !approxEqual(usd, want) {
				t.Errorf("CostForUsage = %g, want %g from the displayed rates", usd, want)
			}
		})
	}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}
