package inference

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func singleUserRow() []domain.Message {
	return []domain.Message{{ID: 1, Role: "user", Content: "hello"}}
}

// TestPricing_ModelMaxOutputAnchors is part of the datasheet verification
// gate the refresh workflow runs (`-run TestPricing`), named for it
// deliberately: the budget policy reads max_output_tokens out of the same
// snapshot the prices come from, so an upstream re-vendor that drops or moves
// the field must fail loudly here rather than silently demote every call to
// the unknown-model fallback.
func TestPricing_ModelMaxOutputAnchors(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		// The two models the native loop actually drives today, plus the
		// Bedrock inference-profile spelling of the first — the region-prefix
		// strip must find the same entry.
		{"claude-opus-5", 128000},
		{"claude-haiku-4-5", 64000},
		{"us.anthropic.claude-opus-5", 128000},
	}
	for _, tc := range cases {
		got, ok := ModelMaxOutput(tc.model)
		if !ok {
			t.Errorf("ModelMaxOutput(%q): not found in the snapshot", tc.model)
			continue
		}
		if got != tc.want {
			t.Errorf("ModelMaxOutput(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}

	if got, ok := ModelMaxOutput("some-model-nobody-ships"); ok {
		t.Errorf("an unknown model must report not-found, got %d", got)
	}
}

func TestMaxOutputTokens_PerProviderPolicy(t *testing.T) {
	t.Run("anthropic direct asks for the model's whole output", func(t *testing.T) {
		if got := MaxOutputTokens(ProviderAnthropic, "claude-opus-5"); got != 128000 {
			t.Errorf("cap = %d, want the model maximum 128000 — a generous cap costs nothing on the direct API", got)
		}
	})

	t.Run("bedrock asks for the budget, not the maximum", func(t *testing.T) {
		// The cap is reserved against the account's per-minute quota at
		// admission, so the model's 128k maximum is exactly what must not be
		// requested.
		if got := MaxOutputTokens(ProviderBedrock, "claude-opus-5"); got != bedrockMaxOutputTokens {
			t.Errorf("cap = %d, want the Bedrock budget %d", got, bedrockMaxOutputTokens)
		}
	})

	t.Run("bedrock never asks for more than the model can emit", func(t *testing.T) {
		// A budget above the model's own maximum is a 400, not a bigger
		// answer — so a model whose ceiling is under the budget pins there.
		if bedrockMaxOutputTokens <= 64000 {
			t.Skip("the Bedrock budget no longer exceeds any model's ceiling; nothing to clamp")
		}
		if got := MaxOutputTokens(ProviderBedrock, "claude-haiku-4-5"); got != 64000 {
			t.Errorf("cap = %d, want the model maximum 64000", got)
		}
	})

	t.Run("an unknown model falls back generously, never to a small constant", func(t *testing.T) {
		got := MaxOutputTokens(ProviderAnthropic, "some-model-nobody-ships")
		if got != defaultMaxOutputTokens {
			t.Errorf("cap = %d, want %d", got, defaultMaxOutputTokens)
		}
		if got <= 4096 {
			t.Errorf("cap = %d — the whole point is that a thinking turn never gets a 4096-token budget", got)
		}
	})

	t.Run("an unknown model on bedrock still gets the budget", func(t *testing.T) {
		if got := MaxOutputTokens(ProviderBedrock, "some-model-nobody-ships"); got != bedrockMaxOutputTokens {
			t.Errorf("cap = %d, want the Bedrock budget %d", got, bedrockMaxOutputTokens)
		}
	})

	t.Run("the two providers are resolved independently", func(t *testing.T) {
		// The whole reason this takes a provider: the same model asks for
		// different amounts depending on who is admitting the request.
		if MaxOutputTokens(ProviderAnthropic, "claude-opus-5") == MaxOutputTokens(ProviderBedrock, "claude-opus-5") {
			t.Error("direct and Bedrock resolved to the same cap; the policy has collapsed to one number")
		}
	})
}

// TestWire_MaxTokensAlwaysSent is the structural half of the invariant: no
// request TF builds can reach the provider layer without a cap, so the layer's
// own small fallback is unreachable from here.
func TestWire_MaxTokensAlwaysSent(t *testing.T) {
	t.Run("an explicit cap rides through unchanged", func(t *testing.T) {
		areq := toAnthropic(t, Request{
			Provider: ProviderAnthropic, Model: "claude-opus-5",
			Rows: singleUserRow(), MaxTokens: 8192,
		})
		if areq.MaxTokens != 8192 {
			t.Errorf("max_tokens = %d, want 8192", areq.MaxTokens)
		}
	})

	t.Run("an unset cap resolves the budget policy", func(t *testing.T) {
		areq := toAnthropic(t, Request{
			Provider: ProviderAnthropic, Model: "claude-opus-5",
			Rows: singleUserRow(),
		})
		if areq.MaxTokens != 128000 {
			t.Errorf("max_tokens = %d, want the resolved policy value 128000", areq.MaxTokens)
		}
	})

	t.Run("the policy follows the provider the call routes to", func(t *testing.T) {
		breq, err := buildChatRequest(Request{
			Provider: ProviderBedrock, Model: "claude-opus-5",
			Rows: singleUserRow(),
		})
		if err != nil {
			t.Fatalf("buildChatRequest: %v", err)
		}
		if breq.Params.MaxCompletionTokens == nil || *breq.Params.MaxCompletionTokens != bedrockMaxOutputTokens {
			t.Fatalf("max_tokens = %v, want the Bedrock budget %d", breq.Params.MaxCompletionTokens, bedrockMaxOutputTokens)
		}
	})
}
