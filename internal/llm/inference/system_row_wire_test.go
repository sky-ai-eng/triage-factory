package inference_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/providers/bedrock"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/inference"
)

// A system row is how a conversation learns its system text changed, and the
// provider binds every thinking block to the top-level system it was produced
// under. So the row has to reach the model after the turns already sent and
// never inside the top-level system, on every provider: as a mid-conversation
// system message where the provider takes one, as a user turn where it
// doesn't.
func TestWire_ASystemRowStaysWhereItIsInTheConversation(t *testing.T) {
	const update = "<instructions_update>the branch convention changed</instructions_update>"
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "go"},
		{ID: 2, Role: "assistant", Content: "working"},
		{ID: 3, Role: "user", Content: "one more thing"},
		{ID: 4, Role: "system", Subtype: domain.MessageSubtypeInjectionSystemUpdate, Content: update},
	}
	for _, tc := range []struct {
		provider schemas.ModelProvider
		model    string
	}{
		{inference.ProviderAnthropic, "claude-fable-5-2"},
		{inference.ProviderBedrock, "us.anthropic.claude-fable-5-2"},
	} {
		t.Run(string(tc.provider), func(t *testing.T) {
			breq, err := inference.BuildChatRequest(inference.Request{
				Provider:       tc.provider,
				Model:          tc.model,
				SystemPrompt:   "framework",
				SystemAddendum: "conversation block",
				Rows:           rows,
			})
			if err != nil {
				t.Fatalf("build the request: %v", err)
			}
			ctx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
			defer cancel()

			var system, messages json.RawMessage
			switch tc.provider {
			case inference.ProviderAnthropic:
				req, err := anthropic.ToAnthropicChatRequest(ctx, breq)
				if err != nil {
					t.Fatalf("convert for Anthropic: %v", err)
				}
				body, _ := json.Marshal(req)
				var sent struct {
					System   json.RawMessage `json:"system"`
					Messages json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(body, &sent); err != nil {
					t.Fatal(err)
				}
				system, messages = sent.System, sent.Messages
			case inference.ProviderBedrock:
				req, err := bedrock.ToBedrockChatCompletionRequest(ctx, breq)
				if err != nil {
					t.Fatalf("convert for Bedrock: %v", err)
				}
				body, _ := json.Marshal(req)
				var sent struct {
					System   json.RawMessage `json:"system"`
					Messages json.RawMessage `json:"messages"`
				}
				if err := json.Unmarshal(body, &sent); err != nil {
					t.Fatal(err)
				}
				system, messages = sent.System, sent.Messages
			}

			if strings.Contains(string(system), "branch convention") {
				t.Fatalf("the update reached the top-level system:\n%s", system)
			}
			var msgs []map[string]any
			if err := json.Unmarshal(messages, &msgs); err != nil {
				t.Fatalf("read the messages: %v", err)
			}
			last, _ := json.Marshal(msgs[len(msgs)-1])
			if !strings.Contains(string(last), "branch convention") {
				t.Fatalf("the update is not the last message:\n%s", messages)
			}
			for _, m := range msgs[:len(msgs)-1] {
				if b, _ := json.Marshal(m); strings.Contains(string(b), "branch convention") {
					t.Fatalf("the update landed ahead of the turns already sent:\n%s", messages)
				}
			}
			if tc.provider == inference.ProviderAnthropic {
				if role, _ := msgs[len(msgs)-1]["role"].(string); role != "system" {
					t.Errorf("on the Claude API the update is a %q message, want a mid-conversation system message", role)
				}
			}
		})
	}
}
