package inference

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/providers/bedrock"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestCapabilityResolver_AnswersFromTheDatasheet: bifrost's capability lookups
// read the embedded datasheet, under the same key rules pricing uses, and a
// model the datasheet does not carry has no record, which bifrost answers with
// its own detection.
func TestCapabilityResolver_AnswersFromTheDatasheet(t *testing.T) {
	for _, tc := range []struct {
		provider schemas.ModelProvider
		model    string
	}{
		{schemas.Anthropic, "claude-opus-5-5"},
		{schemas.Anthropic, "claude-haiku-5-5"},
		{schemas.Bedrock, "us.anthropic.claude-sonnet-5-5"},
	} {
		c := schemas.CapabilitiesFor(tc.provider, tc.model)
		if c == nil {
			t.Fatalf("%s/%s has no capability record", tc.provider, tc.model)
		}
		if c.SupportsAdaptiveThinking == nil || !*c.SupportsAdaptiveThinking {
			t.Errorf("%s: supports_adaptive_thinking = %v, want the datasheet's true", tc.model, c.SupportsAdaptiveThinking)
		}
		if c.SupportsSamplingParams == nil || *c.SupportsSamplingParams {
			t.Errorf("%s: supports_sampling_params = %v, want the datasheet's false", tc.model, c.SupportsSamplingParams)
		}
	}
	if c := schemas.CapabilitiesFor(schemas.Anthropic, "no-such-model"); c != nil {
		t.Errorf("a model the datasheet does not carry has record %+v, want none", c)
	}
}

// TestWire_BedrockShapingReadsTheDatasheet: bifrost's request shaping reads
// the installed record, not only its own detection. The datasheet says the
// Bedrock Claude rows take no assistant prefill, so a conversation ending on
// an assistant turn is sent without it; with no resolver, bifrost keeps it.
func TestWire_BedrockShapingReadsTheDatasheet(t *testing.T) {
	lastRole := func(t *testing.T) string {
		t.Helper()
		breq, err := buildChatRequest(Request{
			Provider: ProviderBedrock, Model: "us.anthropic.claude-sonnet-5-5",
			Rows: []domain.Message{{ID: 1, Role: "user", Content: "hi"}, {ID: 2, Role: "assistant", Content: "Sure,"}},
		})
		if err != nil {
			t.Fatalf("buildChatRequest: %v", err)
		}
		ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
		defer cancel()
		breqOut, err := bedrock.ToBedrockChatCompletionRequest(ctx, breq)
		if err != nil {
			t.Fatalf("ToBedrockChatCompletionRequest: %v", err)
		}
		msgs := breqOut.Messages
		if len(msgs) == 0 {
			t.Fatal("no messages")
		}
		return string(msgs[len(msgs)-1].Role)
	}

	if got := lastRole(t); got != "user" {
		t.Errorf("with the datasheet, the last message is %q, want the prefill left off", got)
	}
	schemas.SetCapabilityResolver(nil)
	t.Cleanup(func() { schemas.SetCapabilityResolver(capabilitiesFor) })
	if got := lastRole(t); got != "assistant" {
		t.Errorf("with no resolver, the last message is %q; this test no longer shows that the record is read", got)
	}
}
