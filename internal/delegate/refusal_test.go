package delegate

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelaccess"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
)

// TestRefusalCause_ClassifiesEveryPlanRefusal holds RefusalCause to each
// refusal PlanDelegation returns, built the way the plan builds it, wrapping
// included, and to the faults beside them: a plain error, a failed read, a
// deadline, and the firing transaction's own refusals, which callers handle
// by name and must not drop as a plan refusal.
func TestRefusalCause_ClassifiesEveryPlanRefusal(t *testing.T) {
	bedrock := modelOn(t, modelcatalog.ProviderBedrock)
	only := func(keys ...string) domain.ModelSet { return domain.TeamModelSet(keys, orgUnrestricted()) }
	_, noDefault := domain.NewTeamModels("", only(domain.ModelSonnet)).RequireDefault()
	_, defaultDisabled := domain.NewTeamModels(domain.ModelHaiku, only(domain.ModelSonnet)).RequireDefault()
	_, pinDisabled := stepModelOrInherit(domain.ModelHaiku, domain.ModelSonnet, only(domain.ModelSonnet))
	noCredentials := modelaccess.For(domain.OrgSettings{LLMAuthMethod: domain.LLMAuthBYOK}, true).Ready()
	unconnected := modelaccess.For(domain.OrgSettings{AnthropicAPIKeyRef: "anthropic_api_key"}, true).Check(bedrock)
	prompt := func(sentinel error) error {
		return fmt.Errorf("resolve step %d prompt for blueprint %q: %w", 0, "Fix CI", fmt.Errorf("%w: %s", sentinel, "p-1"))
	}

	for _, tc := range []struct {
		name  string
		err   error
		cause string
	}{
		{"the org's spend cap", &CostCapError{Scope: CostCapScopeOrg, SpentUSD: 12, CapUSD: 10}, domain.DelegationRefusedSpendCap},
		{"a team's spend cap", &CostCapError{Scope: CostCapScopeTeam, TeamName: "Platform", SpentUSD: 12, CapUSD: 10}, domain.DelegationRefusedSpendCap},
		{"no default model", noDefault, domain.DelegationRefusedNoDefaultModel},
		{"no credentials", fmt.Errorf("cannot dispatch: %w", noCredentials), domain.DelegationRefusedProviderNotConnected},
		{"a provider not connected", fmt.Errorf("cannot dispatch: %w", unconnected), domain.DelegationRefusedProviderNotConnected},
		{"a default not enabled", defaultDisabled, domain.DelegationRefusedModelNotEnabled},
		{"a step pin not enabled", pinDisabled, domain.DelegationRefusedModelNotEnabled},
		{"no blueprint named", fmt.Errorf("%w — select one from the picker", ErrBlueprintUnspecified), domain.DelegationRefusedBlueprintUnusable},
		{"a missing blueprint", fmt.Errorf("%w: %s", ErrBlueprintNotFound, "bp-1"), domain.DelegationRefusedBlueprintUnusable},
		{"a blueprint with no steps", fmt.Errorf("%w: %q", ErrBlueprintNoSteps, "Fix CI"), domain.DelegationRefusedBlueprintUnusable},
		{"no prompt named", prompt(ErrPromptUnspecified), domain.DelegationRefusedBlueprintUnusable},
		{"a missing prompt", prompt(ErrPromptNotFound), domain.DelegationRefusedBlueprintUnusable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatal("the fixture produced no error")
			}
			cause, ok := RefusalCause(tc.err)
			if !ok || cause != tc.cause {
				t.Errorf("RefusalCause(%v) = (%q, %v), want (%q, true)", tc.err, cause, ok, tc.cause)
			}
		})
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"a plain error", errors.New("boom")},
		{"a failed read", fmt.Errorf("failed to load blueprint %s: %w", "bp-1", errors.New("connection reset"))},
		{"a deadline", fmt.Errorf("load blueprint steps: %w", context.DeadlineExceeded)},
		{"a busy task", ErrTaskBusy},
		{"a fenced replay", ErrAlreadyFired},
		{"a closed task", ErrTaskClosed},
		{"an archived team", ErrTeamArchived},
	} {
		t.Run("fault: "+tc.name, func(t *testing.T) {
			if cause, ok := RefusalCause(tc.err); ok {
				t.Errorf("RefusalCause(%v) = (%q, true), want a fault", tc.err, cause)
			}
		})
	}
}
