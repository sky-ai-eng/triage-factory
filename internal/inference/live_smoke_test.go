package inference

import (
	"context"
	"fmt"
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
	if c.Usage.PromptTokens == 0 && c.Usage.OutputTokens == 0 {
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

// TestInference_LiveSmoke_SystemBlockCaching is the acceptance for the split
// system prompt, and it can only be answered by a real provider: whether the
// breakpoint on block 1 writes an entry a DIFFERENT conversation reads is a
// fact about the provider's cache, not about the bytes we assemble.
//
// Two claims, in the two halves below.
//
//  1. Block 1 is shared. Two conversations with different missions — different
//     block 2s, different opening turns — send the same block 1. The second
//     one's FIRST call must read at least block 1's worth of tokens from cache,
//     which is only possible if the entry the first conversation wrote at block
//     1's breakpoint is one this conversation matched. A marker stamped on the
//     last block instead would put that entry after the per-conversation bytes
//     and this read would be zero.
//
//  2. Block 2 is read, not re-written per turn. It carries no marker of its
//     own; the moving conversation breakpoint writes an entry covering it on
//     the first call, so the same conversation's second call reads block 1 AND
//     block 2 together as one longer prefix.
//
// Gated like the smoke above, and it bills three calls. Block 1 has to clear
// the provider's minimum cacheable prefix (1024 tokens on Sonnet) or nothing is
// written at all and both halves fail for a reason that is not TF's.
//
//	TF_TEST_INFERENCE_LIVE=1 ANTHROPIC_API_KEY=sk-... \
//	  go test ./internal/inference -run TestInference_LiveSmoke_SystemBlockCaching -v
func TestInference_LiveSmoke_SystemBlockCaching(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Block 1 stands in for agentprompt.Build: one long, fixed text both
	// conversations send verbatim. Padded past the 1024-token floor with
	// varied prose — a repeated token would compress into a prefix too short
	// to cache, and the test would fail on the padding rather than the policy.
	block1 := cacheablePadding(1500)
	tools := []schemas.ChatTool{addTool()}

	call := func(label, addendum string, rows []domain.Message) *Completion {
		t.Helper()
		out, err := client.Stream(ctx, Request{
			Provider: ProviderAnthropic, Model: model,
			SystemPrompt: block1, SystemAddendum: addendum,
			Rows: rows, Tools: tools, MaxTokens: 256,
		})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		t.Logf("%s: prompt=%d cache_read=%d cache_write=%d",
			label, out.Usage.PromptTokens, out.Usage.CacheReadTokens, out.Usage.CacheCreationTokens)
		return out
	}

	// Conversation A writes the entry at block 1's breakpoint.
	convA := "<run_context>\nRun root: /work\n</run_context>\n\nReview the pull request named above."
	first := call("conversation A, call 1", convA,
		[]domain.Message{{ID: 1, Role: "user", Content: "Say READY and nothing else."}})
	if first.Usage.CacheCreationTokens == 0 && first.Usage.CacheReadTokens == 0 {
		t.Fatalf("no cache activity at all on the first call (%+v) — the provider wrote no entry, so neither claim below is testable", first.Usage)
	}

	// Conversation B: a different mission, a different opening turn, the same
	// block 1. Claim 1 is that its FIRST call still reads block 1.
	convB := "<run_context>\nRun root: /work\n</run_context>\n\nFix the failing check on pull request 18."
	second := call("conversation B, call 1", convB,
		[]domain.Message{{ID: 1, Role: "user", Content: "Say SET and nothing else."}})
	if second.Usage.CacheReadTokens == 0 {
		t.Errorf("a second conversation read nothing from cache (%+v) — block 1's entry is not shared across conversations", second.Usage)
	}

	// Claim 2: the same conversation's next call reads block 1 and block 2
	// together, so the read grows past what block 1 alone accounts for.
	asstRow, err := MessageToRow(second.Message)
	if err != nil {
		t.Fatalf("conversation B MessageToRow: %v", err)
	}
	asstRow.ID = 2
	third := call("conversation B, call 2", convB, []domain.Message{
		{ID: 1, Role: "user", Content: "Say SET and nothing else."},
		asstRow,
		{ID: 3, Role: "user", Content: "Now say GO and nothing else."},
	})
	if third.Usage.CacheReadTokens <= second.Usage.CacheReadTokens {
		t.Errorf("second call read %d cached tokens, first read %d — block 2 is not riding the conversation's own entry",
			third.Usage.CacheReadTokens, second.Usage.CacheReadTokens)
	}
}

// cacheablePadding builds a long, non-repeating filler block. The provider will
// not cache a prefix under ~1024 tokens, and a block of one repeated word does
// not reach that count however long it looks, so the words vary.
func cacheablePadding(words int) string {
	var b strings.Builder
	b.WriteString("You are a test agent. The following notes are filler and carry no instructions.\n")
	for i := 0; i < words; i++ {
		fmt.Fprintf(&b, "Note %d: the quick brown fox jumped over lazy dog number %d in field %d.\n", i, i*7%911, i*13%577)
	}
	return b.String()
}
