package inference

import (
	"context"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// CarriesEffort reports whether a Request's Effort reaches model as
// Anthropic's output_config.effort, unchanged. It runs the same pure
// transform bifrost applies before a call (buildChatRequest, then bifrost's
// Anthropic conversion) on a one-row request and reads what it produced, so
// the answer is the pinned bifrost's own and follows it across an upgrade.
//
// bifrost sends some models a thinking budget in place of an effort, and a
// model that takes only adaptive thinking refuses that budget, so the catalog
// offers a native model only the levels this answers true for. The Bedrock
// conversion decides on the same model predicate, so a Bedrock id answers here
// through the Anthropic conversion.
func CarriesEffort(model, effort string) bool {
	breq, err := buildChatRequest(Request{
		Provider: ProviderAnthropic,
		Model:    model,
		Rows:     []domain.Message{{Role: "user", Content: "."}},
		Effort:   effort,
	})
	if err != nil {
		return false
	}
	bfCtx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	areq, err := anthropic.ToAnthropicChatRequest(bfCtx, breq)
	if err != nil || areq == nil || areq.OutputConfig == nil || areq.OutputConfig.Effort == nil {
		return false
	}
	return *areq.OutputConfig.Effort == effort
}
