package inference

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestInference_LiveSmoke is a live end-to-end smoke against a real provider.
// Skipped by default; opt in with a key and the gate:
//
//	TF_TEST_INFERENCE_LIVE=1 ANTHROPIC_API_KEY=sk-... \
//	  go test ./internal/inference -run TestInference_LiveSmoke -v
//
// It runs a 3-turn tool conversation on sonnet-5 (override with
// TF_TEST_INFERENCE_MODEL): the assistant thinks (signed) and calls a tool,
// we feed the tool result back, and the second call replays the signed
// reasoning — proving signatures round-trip and replay is accepted — with
// usage recorded on every turn. Bills against ANTHROPIC_API_KEY.
func TestInference_LiveSmoke(t *testing.T) {
	if os.Getenv("TF_TEST_INFERENCE_LIVE") != "1" {
		t.Skip("set TF_TEST_INFERENCE_LIVE=1 to run the live inference smoke test")
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("set ANTHROPIC_API_KEY to run the live inference smoke test")
	}
	model := os.Getenv("TF_TEST_INFERENCE_MODEL")
	if model == "" {
		model = "claude-sonnet-5"
	}

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderAnthropic,
		APIKey:   apiKey,
		BaseURL:  os.Getenv("ANTHROPIC_BASE_URL"),
		Models:   []string{model},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const systemPrompt = "You are a calculator agent. When asked to add numbers, call the add tool."
	tools := []schemas.ChatTool{addTool()}

	// Turn 1: the model should think, then call the add tool.
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "Please compute 40 + 2 using the add tool."},
	}
	first, err := client.Stream(ctx, Request{
		Provider: ProviderAnthropic, Model: model,
		SystemPrompt: systemPrompt, Rows: rows, Tools: tools, Effort: "low", MaxTokens: 2048,
	})
	if err != nil {
		t.Fatalf("turn 1 stream: %v", err)
	}
	assertUsageRecorded(t, "turn 1", first)

	asstRow, err := MessageToRow(first.Message)
	if err != nil {
		t.Fatalf("turn 1 MessageToRow: %v", err)
	}
	asstRow.ID = 2
	if len(asstRow.ToolCalls) == 0 {
		t.Fatalf("turn 1: expected a tool call, got finish=%q content=%q", first.FinishReason, asstRow.Content)
	}
	if signed := countSignedReasoning(asstRow.Reasoning); signed == 0 {
		t.Logf("turn 1: no signed reasoning returned (model may not have engaged thinking)")
	} else {
		t.Logf("turn 1: %d signed reasoning block(s) captured", signed)
	}
	rows = append(rows, asstRow)

	// Feed a tool result back for every call the model made.
	nextID := 3
	for _, call := range asstRow.ToolCalls {
		rows = append(rows, domain.Message{
			ID: nextID, Role: "tool", Subtype: "tool", ToolCallID: call.ID, Content: "42",
		})
		nextID++
	}

	// Turn 2: replay the transcript, including the signed reasoning. Acceptance
	// here IS the signature-replay proof.
	second, err := client.Stream(ctx, Request{
		Provider: ProviderAnthropic, Model: model,
		SystemPrompt: systemPrompt, Rows: rows, Tools: tools, Effort: "low", MaxTokens: 2048,
	})
	if err != nil {
		t.Fatalf("turn 2 stream (signature replay rejected?): %v", err)
	}
	assertUsageRecorded(t, "turn 2", second)

	finalRow, err := MessageToRow(second.Message)
	if err != nil {
		t.Fatalf("turn 2 MessageToRow: %v", err)
	}
	if !strings.Contains(finalRow.Content, "42") {
		t.Logf("turn 2 final answer did not mention 42 (non-fatal): %q", finalRow.Content)
	}
	t.Logf("live smoke ok: model=%s finish=%q final=%q", second.Model, second.FinishReason, finalRow.Content)
}

// TestInference_LiveSmoke_Bedrock is the separately-gated live Bedrock call.
// It reuses the Bedrock STS/keys config accepted at Init (covered offline by
// TestNew_InitAcceptsAccounts) and makes one real call.
//
//	TF_TEST_INFERENCE_BEDROCK_LIVE=1 AWS_REGION=us-east-1 \
//	  AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
//	  go test ./internal/inference -run TestInference_LiveSmoke_Bedrock -v
func TestInference_LiveSmoke_Bedrock(t *testing.T) {
	if os.Getenv("TF_TEST_INFERENCE_BEDROCK_LIVE") != "1" {
		t.Skip("set TF_TEST_INFERENCE_BEDROCK_LIVE=1 to run the live Bedrock smoke test")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		t.Skip("set AWS_REGION to run the live Bedrock smoke test")
	}
	model := os.Getenv("TF_TEST_INFERENCE_BEDROCK_MODEL")
	if model == "" {
		model = "us.anthropic.claude-sonnet-4-20250514-v1:0"
	}

	acct, err := NewAccount(ProviderCredentials{
		Provider: ProviderBedrock,
		Models:   []string{model},
		Bedrock: &BedrockCredentials{
			Region:       region,
			AccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
			SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
			RoleARN:      os.Getenv("TF_TEST_BEDROCK_ROLE_ARN"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(acct)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	out, err := client.Stream(ctx, Request{
		Provider: ProviderBedrock, Model: model,
		Rows:      []domain.Message{{ID: 1, Role: "user", Content: "Reply with exactly PONG."}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("bedrock stream: %v", err)
	}
	assertUsageRecorded(t, "bedrock", out)
	row, _ := MessageToRow(out.Message)
	t.Logf("bedrock live smoke ok: %q", row.Content)
}

func assertUsageRecorded(t *testing.T, label string, c *Completion) {
	t.Helper()
	if c.Usage.InputTokens == 0 && c.Usage.OutputTokens == 0 {
		t.Errorf("%s: expected usage to be recorded, got %+v", label, c.Usage)
	}
	if usd, ok := CostForUsage(c.Model, c.Usage); ok {
		t.Logf("%s: usage=%+v cost=$%.6f", label, c.Usage, usd)
	} else {
		t.Logf("%s: usage=%+v (model %q not in pricing snapshot)", label, c.Usage, c.Model)
	}
}

func countSignedReasoning(details []domain.ReasoningDetail) int {
	n := 0
	for _, d := range details {
		if d.Signature != "" {
			n++
		}
	}
	return n
}

// addTool is a trivial two-integer add tool for the live smoke conversation.
func addTool() schemas.ChatTool {
	props := schemas.NewOrderedMap()
	props.Set("a", map[string]any{"type": "integer"})
	props.Set("b", map[string]any{"type": "integer"})
	desc := "Add two integers and return the sum."
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        "add",
			Description: &desc,
			Parameters: &schemas.ToolFunctionParameters{
				Type:       "object",
				Properties: props,
				Required:   []string{"a", "b"},
			},
		},
	}
}
