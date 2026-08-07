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
	const bedrockBudget = 32768

	t.Run("anthropic direct asks for the model's whole output", func(t *testing.T) {
		if got := maxOutputTokens(ProviderAnthropic, "claude-opus-5", bedrockBudget); got != 128000 {
			t.Errorf("cap = %d, want the model maximum 128000 — a generous cap costs nothing on the direct API", got)
		}
	})

	t.Run("bedrock asks for the budget, not the maximum", func(t *testing.T) {
		// The cap is reserved against the account's per-minute quota at
		// admission, so the model's 128k maximum is exactly what must not be
		// requested.
		if got := maxOutputTokens(ProviderBedrock, "claude-opus-5", bedrockBudget); got != bedrockBudget {
			t.Errorf("cap = %d, want the Bedrock budget %d", got, bedrockBudget)
		}
	})

	t.Run("bedrock never asks for more than the model can emit", func(t *testing.T) {
		// A budget above the model's own maximum is a 400, not a bigger
		// answer.
		if got := maxOutputTokens(ProviderBedrock, "claude-haiku-4-5", 100000); got != 64000 {
			t.Errorf("cap = %d, want the model maximum 64000", got)
		}
	})

	t.Run("an unknown model falls back generously, never to a small constant", func(t *testing.T) {
		got := maxOutputTokens(ProviderAnthropic, "some-model-nobody-ships", bedrockBudget)
		if got != DefaultMaxOutputTokens {
			t.Errorf("cap = %d, want %d", got, DefaultMaxOutputTokens)
		}
		if got <= 4096 {
			t.Errorf("cap = %d — the whole point is that a thinking turn never gets a 4096-token budget", got)
		}
	})

	t.Run("an unknown model on bedrock still gets the budget", func(t *testing.T) {
		if got := maxOutputTokens(ProviderBedrock, "some-model-nobody-ships", bedrockBudget); got != bedrockBudget {
			t.Errorf("cap = %d, want the Bedrock budget %d", got, bedrockBudget)
		}
	})
}

func TestMaxOutputTokens_BedrockBudgetEnvOverride(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv(BedrockMaxOutputEnv, "")
		if got := MaxOutputTokens(ProviderBedrock, "claude-opus-5"); got != DefaultBedrockMaxOutputTokens {
			t.Errorf("cap = %d, want %d", got, DefaultBedrockMaxOutputTokens)
		}
	})

	t.Run("honored when set", func(t *testing.T) {
		t.Setenv(BedrockMaxOutputEnv, "8192")
		if got := MaxOutputTokens(ProviderBedrock, "claude-opus-5"); got != 8192 {
			t.Errorf("cap = %d, want the override 8192", got)
		}
		// Anthropic direct is a different budget and must not move with it.
		if got := MaxOutputTokens(ProviderAnthropic, "claude-opus-5"); got != 128000 {
			t.Errorf("direct cap = %d, want 128000 — the Bedrock override is Bedrock's alone", got)
		}
	})

	t.Run("a junk value falls back rather than sending nonsense", func(t *testing.T) {
		for _, raw := range []string{"lots", "0", "-1"} {
			t.Setenv(BedrockMaxOutputEnv, raw)
			if got := MaxOutputTokens(ProviderBedrock, "claude-opus-5"); got != DefaultBedrockMaxOutputTokens {
				t.Errorf("%q: cap = %d, want the default %d", raw, got, DefaultBedrockMaxOutputTokens)
			}
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
		t.Setenv(BedrockMaxOutputEnv, "")
		breq, err := buildChatRequest(Request{
			Provider: ProviderBedrock, Model: "claude-opus-5",
			Rows: singleUserRow(),
		})
		if err != nil {
			t.Fatalf("buildChatRequest: %v", err)
		}
		if breq.Params.MaxCompletionTokens == nil || *breq.Params.MaxCompletionTokens != DefaultBedrockMaxOutputTokens {
			t.Fatalf("max_tokens = %v, want the Bedrock budget %d", breq.Params.MaxCompletionTokens, DefaultBedrockMaxOutputTokens)
		}
	})
}
