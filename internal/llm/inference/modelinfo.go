package inference

// ModelInfo is the read side of the embedded datasheet: the per-model facts a
// caller needs to *describe* a model rather than to bill one. It exists because
// the snapshot is embedded here and nothing else may re-embed it — a second
// copy of a 2,400-model file would drift from the one the ledger prices
// against, and the whole point of a catalog join is that the price a user is
// shown is the price they are charged.

// Info is one model's datasheet row, projected onto the fields a describing
// caller reads.
//
// Every rate is the BASE (≤200k prompt) per-token USD rate — the same number
// computeTextCost applies to a request under the long-context boundary, so a
// price rendered from this Info is a price the ledger would really charge.
// Requests over that boundary bill at the model's *_above_200k tier where it
// publishes one; CostForUsage remains the only authority on what a specific
// request cost.
//
// A zero cache rate is a fact about the vendor, not a gap: several vendors
// populate a cache for free, and that is exactly what tieredRate charges for
// them.
type Info struct {
	// Provider is the datasheet's litellm_provider — an upstream vendor
	// label, not a bifrost provider constant. Mapping the two is
	// provider_map.json's job, and it is a separate lookup on purpose: this
	// value is what the snapshot says, and a caller that needs the bifrost
	// side should map it rather than receive a silently translated string.
	Provider string

	// MaxInputTokens is the model's context window (input side), matching
	// what ModelWindow reports. Zero when the row records no window.
	MaxInputTokens int

	// SupportsPromptCaching reports the row's supports_prompt_caching flag.
	// It is surfaced, never gated on: a model without it still runs, it just
	// costs several times more under TF's cacheable-prefix design, and the
	// person choosing it should see that before they choose.
	SupportsPromptCaching bool

	InputCostPerToken      float64
	OutputCostPerToken     float64
	CacheReadCostPerToken  float64
	CacheWriteCostPerToken float64
}

// ModelInfo resolves a model id to its datasheet row, with the same lookup
// rules as pricing (exact id, then a single Bedrock region-prefix strip) so a
// caller describing a model and the ledger billing it always read the same row.
//
// ok is false in two cases. The snapshot doesn't carry the key — nothing is
// known about it. Or the row carries no input or output rate: an unpriced model
// must not be offered, and since a nil rate bills as 0, reporting one would
// render a model as free instead of as unavailable.
func ModelInfo(model string) (Info, bool) {
	table, err := loadPricing()
	if err != nil || table == nil {
		return Info{}, false
	}
	p, ok := lookupPrice(table, model)
	if !ok || p.InputCostPerToken == nil || p.OutputCostPerToken == nil {
		return Info{}, false
	}
	info := Info{
		Provider:               p.LiteLLMProvider,
		SupportsPromptCaching:  p.SupportsPromptCaching,
		InputCostPerToken:      *p.InputCostPerToken,
		OutputCostPerToken:     *p.OutputCostPerToken,
		CacheReadCostPerToken:  deref(p.CacheReadInputTokenCost),
		CacheWriteCostPerToken: deref(p.CacheCreationInputTokenCost),
	}
	if p.MaxInputTokens != nil && *p.MaxInputTokens > 0 {
		info.MaxInputTokens = int(*p.MaxInputTokens)
	}
	return info, true
}

// deref reads an optional rate, treating an absent one as 0 — the same value
// tieredRate charges for it, so a described price and a billed price agree.
func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
