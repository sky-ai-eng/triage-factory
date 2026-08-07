package inference

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
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
// the current Claude generation) — so an oversized cap throttles requests that
// would have fit and collapses effective concurrency. AWS's own guidance is to
// "set max_tokens to approximate your expected completion size", which is what
// the budget below is.
//
// Both arms exist to make one thing unreachable: a cap nobody chose. An unset
// cap is filled by the provider layer with a small constant, and a
// thinking-heavy turn spends that entire constant on reasoning and returns
// nothing — no text, no tool call, no work.

const (
	// DefaultMaxOutputTokens is the cap for a model the datasheet does not
	// carry. It is deliberately large: Anthropic's own guidance for
	// high-effort thinking is to allow at least this much, and the failure
	// this constant exists to prevent is a turn whose whole budget went to
	// reasoning. It is never the provider layer's small fallback.
	DefaultMaxOutputTokens = 65536

	// DefaultBedrockMaxOutputTokens is the Bedrock budget: large enough that
	// a thinking-heavy turn clears it, small enough that the admission
	// reservation does not collapse concurrency at a 10x burndown. Tunable
	// per deployment via BedrockMaxOutputEnv against observed throttling
	// versus observed cap hits.
	DefaultBedrockMaxOutputTokens = 32768

	// BedrockMaxOutputEnv overrides the Bedrock budget. A configured value is
	// used as given — it is the deployment's quota being spent, and a knob
	// that silently substitutes a different number is not a knob. The only
	// bound applied to it is the model's own maximum, above which the API
	// rejects the request outright. There is deliberately NO lower floor: a
	// value small enough that a thinking-heavy turn exhausts it before acting
	// will reproduce the failure the default guards against, and the engine's
	// output-limit arm is what makes that legible rather than silent.
	//
	// An unparseable or non-positive value is not a configured value at all —
	// it names no budget — so it warns once and falls back to the default.
	BedrockMaxOutputEnv = "TF_BEDROCK_MAX_OUTPUT_TOKENS"
)

var maxTokensLog = logging.Component("inference")

// badBedrockBudgetWarn keeps the invalid-value warning to one line per
// process. The read below happens per provider call, so an unguarded warn
// would log on every LLM call for the lifetime of a misconfigured deployment
// — turning one configuration mistake into a permanent stream of identical
// lines, which is how the line stops being read at all.
var badBedrockBudgetWarn sync.Once

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
	return maxOutputTokens(provider, model, bedrockOutputBudget())
}

// maxOutputTokens is the policy proper, with the deployment's Bedrock budget
// as a parameter so both arms are testable without touching the environment.
func maxOutputTokens(provider schemas.ModelProvider, model string, bedrockBudget int) int {
	ceiling, known := ModelMaxOutput(model)

	if provider == ProviderBedrock {
		if bedrockBudget <= 0 {
			bedrockBudget = DefaultBedrockMaxOutputTokens
		}
		// The budget is what we ask for, whatever the operator set it to; the
		// model's own maximum is the one bound, because asking above it is a
		// 400 rather than a bigger answer. Nothing raises a low budget — see
		// BedrockMaxOutputEnv for why that is deliberate.
		if known && ceiling < bedrockBudget {
			return ceiling
		}
		return bedrockBudget
	}

	if known {
		return ceiling
	}
	return DefaultMaxOutputTokens
}

// bedrockOutputBudget reads the deployment's Bedrock budget. Read per call
// rather than cached: it is one env lookup next to a provider round trip, and
// caching it would make the value a process-lifetime fact that an operator
// cannot correct without a restart.
func bedrockOutputBudget() int {
	raw := strings.TrimSpace(os.Getenv(BedrockMaxOutputEnv))
	if raw == "" {
		return DefaultBedrockMaxOutputTokens
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		badBedrockBudgetWarn.Do(func() {
			maxTokensLog.Warn("invalid "+BedrockMaxOutputEnv+"; using the default Bedrock output budget",
				"value", raw, "default", DefaultBedrockMaxOutputTokens)
		})
		return DefaultBedrockMaxOutputTokens
	}
	return n
}
