package delegate

import (
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
)

// RefusalCause reports whether err is one of PlanDelegation's refusals, and
// which: one of the domain.DelegationRefused* causes. A refusal is a standing
// condition: planning the same delegation again is refused the same way until
// the setting it names changes, or, for a spend cap, the UTC day rolls over.
//
// Anything else answers ok=false and is a fault: a store read that failed, a
// deadline, or a firing transaction that failed. A fault can come out
// differently on a retry and a refusal cannot, which is the line every caller
// that fires a trigger draws between the two.
func RefusalCause(err error) (cause string, ok bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrDailyCostCapReached):
		return domain.DelegationRefusedSpendCap, true
	case errors.Is(err, domain.ErrNoDefaultModel):
		return domain.DelegationRefusedNoDefaultModel, true
	case errors.Is(err, modelaccess.ErrNoCredentials), errors.Is(err, modelaccess.ErrProviderUnconfigured):
		return domain.DelegationRefusedProviderNotConnected, true
	case errors.Is(err, domain.ErrModelNotEnabled):
		return domain.DelegationRefusedModelNotEnabled, true
	case errors.Is(err, ErrBlueprintNotFound), errors.Is(err, ErrBlueprintUnspecified),
		errors.Is(err, ErrBlueprintNoSteps), errors.Is(err, ErrPromptNotFound),
		errors.Is(err, ErrPromptUnspecified):
		return domain.DelegationRefusedBlueprintUnusable, true
	}
	return "", false
}
