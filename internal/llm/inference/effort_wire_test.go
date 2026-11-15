package inference_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/providers/bedrock"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
)

// sentEffort is what an effort becomes in the request bifrost sends:
// Anthropic's thinking and output_config, which the Bedrock request carries
// in additionalModelRequestFields.
type sentEffort struct {
	Thinking *struct {
		Type         string `json:"type"`
		BudgetTokens *int   `json:"budget_tokens"`
	} `json:"thinking"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
}

// TestWire_EveryOfferedEffortReachesTheModel checks that the efforts the
// catalog offers, which it reads from the datasheet, and the requests bifrost
// builds agree. For every native model TF offers and every level it is
// offered, the request carries the level unchanged as output_config.effort,
// with adaptive thinking and no thinking budget, which a model that takes only
// adaptive thinking refuses. The request checked is bifrost's own conversion
// for the model's provider, the last step before it is sent, so a datasheet
// refresh or a bifrost bump that puts the two out of step fails here rather
// than on a person's message.
func TestWire_EveryOfferedEffortReachesTheModel(t *testing.T) {
	checked := 0
	for _, m := range modelcatalog.UniverseFor(true).Models() {
		for _, level := range m.EffortLevels {
			checked++
			t.Run(m.Key+"/"+level, func(t *testing.T) {
				got := effortSent(t, m, level)
				if got.Thinking == nil || got.Thinking.Type != "adaptive" || got.Thinking.BudgetTokens != nil {
					t.Errorf("thinking = %+v, want adaptive with no budget", got.Thinking)
				}
				if got.OutputConfig == nil || got.OutputConfig.Effort != level {
					t.Errorf("output_config = %+v, want effort %q", got.OutputConfig, level)
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("the catalog offers no native model an effort, so nothing was checked")
	}
}

// effortSent converts a one-message request at level for m the way bifrost
// does for m's provider, and reads the effort fields from the result.
func effortSent(t *testing.T, m modelcatalog.Model, level string) sentEffort {
	t.Helper()
	provider := schemas.ModelProvider(m.Provider)
	breq, err := inference.BuildChatRequest(inference.Request{
		Provider: provider,
		Model:    m.Key,
		Rows:     []domain.Message{{ID: 1, Role: "user", Content: "hi"}},
		Effort:   level,
	})
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()

	var fields []byte
	switch provider {
	case inference.ProviderAnthropic:
		req, err := anthropic.ToAnthropicChatRequest(ctx, breq)
		if err != nil {
			t.Fatalf("convert for Anthropic: %v", err)
		}
		if fields, err = json.Marshal(req); err != nil {
			t.Fatal(err)
		}
	case inference.ProviderBedrock:
		req, err := bedrock.ToBedrockChatCompletionRequest(ctx, breq)
		if err != nil {
			t.Fatalf("convert for Bedrock: %v", err)
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var sent struct {
			Fields json.RawMessage `json:"additionalModelRequestFields"`
		}
		if err := json.Unmarshal(body, &sent); err != nil {
			t.Fatal(err)
		}
		fields = sent.Fields
	default:
		t.Fatalf("no conversion to check for provider %q", m.Provider)
	}
	var got sentEffort
	if len(fields) > 0 {
		if err := json.Unmarshal(fields, &got); err != nil {
			t.Fatalf("read the effort fields: %v (%s)", err, fields)
		}
	}
	return got
}
