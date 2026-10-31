package inference

import (
	"github.com/maximhq/bifrost/core/schemas"
)

// The completion cap — `max_tokens` on the wire — is a per-provider budget,
// not one number.
//
// Anthropic direct bills output tokens on what was actually generated: the
// rate-limit docs are explicit that the parameter "does not factor into OTPM
// rate limit calculations, so there is no rate limit downside to setting a
// higher max_tokens value". A generous cap there costs nothing and buys the
// headroom a high-effort thinking turn needs before it has produced a single
// token of answer.
//
// Bedrock charges for the ask. AWS deducts `input + max_tokens` from the
// account's per-minute token quota at admission, adjusts during processing,
// and replenishes at the end against the model's burndown multiplier (10x for
// the current Claude generation). AWS's own guidance is to "set max_tokens to
// approximate your expected completion size", which is what the Bedrock
// constant below is.
//
// Both arms exist to make one thing unreachable: a cap nobody chose. An unset
// cap is filled by the provider layer with a small constant, and a
// thinking-heavy turn spends that entire constant on reasoning and returns
// nothing — no text, no tool call, no work.

const (
	// defaultMaxOutputTokens is the cap for a model the datasheet does not
	// carry. It is deliberately large: Anthropic's own guidance for
	// high-effort thinking is to allow at least this much, and the failure
	// this constant exists to prevent is a turn whose whole budget went to
	// reasoning. It is never the provider layer's small fallback.
	defaultMaxOutputTokens = 65536

	// bedrockMaxOutputTokens is the Bedrock budget: large enough that a
	// thinking-heavy turn clears it, small enough that the admission
	// reservation is not the thing bounding concurrency.
	//
	// It is a constant and not a deployment knob, deliberately. On an agent
	// loop the admission reservation is `input + max_tokens` and the input
	// side dominates it — a turn carrying a 100k-token transcript reserves
	// ~133k here versus ~108k at a quarter of this budget, so tuning it moves
	// a secondary term by a fraction. What actually burns the quota is the
	// settlement, `output × burndown`, and that is driven by tokens the model
	// really generated, which this number does not control. A knob whose two
	// failure directions are "throttled slightly sooner" and "every
	// thinking-heavy turn wastes a call and retries" is not worth a line of
	// deployment configuration.
	//
	// If a real Bedrock deployment does show ThrottlingExceptions traceable
	// to the reservation, lowering this is a one-line change — and the
	// evidence for what to lower it TO would arrive with the report, which is
	// exactly the thing a knob invented in advance cannot have.
	bedrockMaxOutputTokens = 32768
)

// ModelMaxOutput returns the model's maximum completion length from the
// vendored datasheet, with the same lookup rules as pricing and ModelWindow
// (exact id, then a single Bedrock region-prefix strip). ok is false for a
// model the snapshot doesn't carry or one with no recorded maximum — the
// caller must fall back to a policy default, never guess a number.
func ModelMaxOutput(model string) (int, bool) {
	table, err := loadPricing()
	if err != nil || table == nil {
		return 0, false
	}
	price, ok := lookupPrice(table, model)
	if !ok || price.MaxOutputTokens == nil || *price.MaxOutputTokens <= 0 {
		return 0, false
	}
	return int(*price.MaxOutputTokens), true
}

// MaxOutputTokens resolves the completion cap for one call, given the provider
// it is routed to and the model it requests. It is the single policy every
// call site shares — including the request builder's own backstop, which is
// what makes the provider layer's default unreachable from TF.
func MaxOutputTokens(provider schemas.ModelProvider, model string) int {
	ceiling, known := ModelMaxOutput(model)

	if provider == ProviderBedrock {
		// The budget is what we ask for; the model's own maximum is the one
		// bound, because asking above it is a 400 rather than a bigger answer.
		if known && ceiling < bedrockMaxOutputTokens {
			return ceiling
		}
		return bedrockMaxOutputTokens
	}

	if known {
		return ceiling
	}
	return defaultMaxOutputTokens
}
