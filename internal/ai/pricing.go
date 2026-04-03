package ai

// Per-million-token pricing in USD, sourced from https://claude.com/pricing#api
// Updated: 2026-04-03
//
// Claude Code uses 5-minute cache TTL (1.25x base input price for cache writes).

type modelPricing struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64 // 5-min TTL (1.25x input)
}

var pricing = map[string]modelPricing{
	"claude-opus-4-6": {
		InputPerMTok:      5.00,
		OutputPerMTok:     25.00,
		CacheReadPerMTok:  0.50,
		CacheWritePerMTok: 6.25,
	},
	"claude-sonnet-4-6": {
		InputPerMTok:      3.00,
		OutputPerMTok:     15.00,
		CacheReadPerMTok:  0.30,
		CacheWritePerMTok: 3.75,
	},
	"claude-haiku-4-5-20251001": {
		InputPerMTok:      1.00,
		OutputPerMTok:     5.00,
		CacheReadPerMTok:  0.10,
		CacheWritePerMTok: 1.25,
	},
	// Short aliases (as stored in config/run model field)
	"opus": {
		InputPerMTok:      5.00,
		OutputPerMTok:     25.00,
		CacheReadPerMTok:  0.50,
		CacheWritePerMTok: 6.25,
	},
	"sonnet": {
		InputPerMTok:      3.00,
		OutputPerMTok:     15.00,
		CacheReadPerMTok:  0.30,
		CacheWritePerMTok: 3.75,
	},
	"haiku": {
		InputPerMTok:      1.00,
		OutputPerMTok:     5.00,
		CacheReadPerMTok:  0.10,
		CacheWritePerMTok: 1.25,
	},
}

// CalculateCostUSD computes the dollar cost from token counts and a model identifier.
// This may slightly underestimate for models with extended thinking (thinking tokens
// aren't broken out in per-message usage). Returns 0 if the model is unknown.
func CalculateCostUSD(model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) float64 {
	p, ok := pricing[model]
	if !ok {
		return 0
	}
	cost := float64(inputTokens) * p.InputPerMTok / 1_000_000
	cost += float64(outputTokens) * p.OutputPerMTok / 1_000_000
	cost += float64(cacheReadTokens) * p.CacheReadPerMTok / 1_000_000
	cost += float64(cacheCreationTokens) * p.CacheWritePerMTok / 1_000_000
	return cost
}
