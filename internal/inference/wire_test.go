package inference

import (
	"bytes"
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// toAnthropic assembles a Request into its exact Anthropic wire payload with no
// network call — the offline equivalent of bifrost's SendBackRawRequest: the
// same pure provider transform bifrost runs before hitting the wire.
func toAnthropic(t *testing.T, req Request) *anthropic.AnthropicMessageRequest {
	t.Helper()
	breq, err := buildChatRequest(req)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	bfCtx, cancel := schemas.NewBifrostContextWithCancel(context.Background())
	defer cancel()
	areq, err := anthropic.ToAnthropicChatRequest(bfCtx, breq)
	if err != nil {
		t.Fatalf("ToAnthropicChatRequest: %v", err)
	}
	return areq
}

// toolConversationRows is a 3-turn tool conversation: a user ask, an assistant
// turn that thinks (signed) then calls two tools, the two tool results (one
// errored), and a final assistant answer.
func toolConversationRows() []domain.Message {
	return []domain.Message{
		{ID: 1, Role: "user", Content: "check the two files"},
		{ID: 2, Role: "assistant", Content: "I'll read both.",
			Reasoning: []domain.ReasoningDetail{{
				Index: 0, Type: "reasoning.text",
				Text: "Read them in parallel.", Signature: "c2lnLTE=",
			}},
			ToolCalls: []domain.ToolCall{
				{ID: "toolu_a", Name: "Read", Input: map[string]any{"path": "/a"}},
				{ID: "toolu_b", Name: "Read", Input: map[string]any{"path": "/b"}},
			},
		},
		{ID: 3, Role: "tool", ToolCallID: "toolu_a", Content: "contents of a"},
		{ID: 4, Role: "tool", ToolCallID: "toolu_b", Content: "no such file", IsError: true},
		{ID: 5, Role: "assistant", Content: "One file was missing."},
	}
}

func TestWire_ThinkingBlockFirstWithSignature(t *testing.T) {
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		SystemPrompt: "You are a triage agent.",
		Rows:         toolConversationRows(),
	})

	asst := findAssistant(t, areq, "I'll read both.")
	blocks := asst.Content.ContentBlocks
	if len(blocks) == 0 {
		t.Fatal("assistant message has no content blocks")
	}
	// The thinking block must come first, and carry its signature.
	if blocks[0].Type != anthropic.AnthropicContentBlockTypeThinking {
		t.Fatalf("first block must be thinking, got %q", blocks[0].Type)
	}
	if blocks[0].Signature == nil || *blocks[0].Signature != "c2lnLTE=" {
		t.Fatalf("thinking block must carry its signature, got %+v", blocks[0].Signature)
	}
	// A tool_use block must come after the thinking block.
	sawToolUseAfterThinking := false
	for i, b := range blocks {
		if b.Type == anthropic.AnthropicContentBlockTypeToolUse {
			if i == 0 {
				t.Fatal("tool_use must not precede the thinking block")
			}
			sawToolUseAfterThinking = true
		}
	}
	if !sawToolUseAfterThinking {
		t.Fatal("expected a tool_use block after the thinking block")
	}
}

func TestWire_ConsecutiveToolResultsPackedIntoOneUserMessage(t *testing.T) {
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		Rows: toolConversationRows(),
	})

	// The two consecutive tool rows (ids 3 and 4) must collapse into a single
	// user message carrying two tool_result blocks — bifrost owns this packing.
	var toolResultMsgs int
	for _, m := range areq.Messages {
		if m.Role != anthropic.AnthropicMessageRoleUser {
			continue
		}
		var toolResults int
		for _, b := range m.Content.ContentBlocks {
			if b.Type == anthropic.AnthropicContentBlockTypeToolResult {
				toolResults++
			}
		}
		if toolResults > 0 {
			toolResultMsgs++
			if toolResults != 2 {
				t.Fatalf("expected 2 tool_result blocks packed into one user message, got %d", toolResults)
			}
		}
	}
	if toolResultMsgs != 1 {
		t.Fatalf("expected exactly one user message carrying tool results, got %d", toolResultMsgs)
	}
}

func TestWire_IsErrorOnToolResult(t *testing.T) {
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		Rows: toolConversationRows(),
	})

	var errFound, okFound bool
	for _, m := range areq.Messages {
		for _, b := range m.Content.ContentBlocks {
			if b.Type != anthropic.AnthropicContentBlockTypeToolResult {
				continue
			}
			switch *b.ToolUseID {
			case "toolu_b":
				if b.IsError == nil || !*b.IsError {
					t.Fatalf("errored tool result (toolu_b) must carry is_error=true, got %+v", b.IsError)
				}
				errFound = true
			case "toolu_a":
				if b.IsError != nil && *b.IsError {
					t.Fatal("successful tool result (toolu_a) must not carry is_error=true")
				}
				okFound = true
			}
		}
	}
	if !errFound || !okFound {
		t.Fatalf("expected both tool results on the wire (ok=%v err=%v)", okFound, errFound)
	}
}

// TestWire_SeqAnchoredToolResultLandsImmediatelyAfterItsCall pins the wire
// shape a repaired transcript produces when input arrived between a tool call
// and the result that answers it. The result row carries a fractional seq
// placing it under its call, so it must reach the wire in the message directly
// after the assistant turn — the user rows that outrank it by id follow behind.
// Anthropic rejects any other arrangement outright ("tool_use ids were found
// without tool_result blocks immediately after"), which is why the placement
// exists at all.
func TestWire_SeqAnchoredToolResultLandsImmediatelyAfterItsCall(t *testing.T) {
	anchored := 2.5
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		Rows: []domain.Message{
			{ID: 1, Role: "user", Content: "check the file"},
			{ID: 2, Role: "assistant", ToolCalls: []domain.ToolCall{
				{ID: "toolu_a", Name: "Read", Input: map[string]any{"path": "/a"}},
			}},
			{ID: 3, Role: "user", Content: "also update the changelog"},
			{ID: 4, Role: "user", Subtype: domain.MessageSubtypeStopNote, Content: "This run was stopped by the user."},
			{ID: 5, Role: "tool", ToolCallID: "toolu_a", Content: "interrupted", IsError: true, Seq: &anchored},
		},
	})

	assistantAt := -1
	for i, m := range areq.Messages {
		for _, b := range m.Content.ContentBlocks {
			if b.Type == anthropic.AnthropicContentBlockTypeToolUse {
				assistantAt = i
			}
		}
	}
	if assistantAt < 0 {
		t.Fatal("no tool_use block reached the wire")
	}
	if assistantAt == len(areq.Messages)-1 {
		t.Fatal("the tool_use is the final message; nothing answers it")
	}
	next := areq.Messages[assistantAt+1]
	if next.Role != anthropic.AnthropicMessageRoleUser {
		t.Fatalf("message after the tool_use has role %q, want the user turn carrying its result", next.Role)
	}
	var answered bool
	for _, b := range next.Content.ContentBlocks {
		if b.Type == anthropic.AnthropicContentBlockTypeToolResult && b.ToolUseID != nil && *b.ToolUseID == "toolu_a" {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("the message after the tool_use carries no tool_result for it: %+v", next.Content)
	}
}

func TestWire_CacheControlPlacement(t *testing.T) {
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		SystemPrompt: "You are a triage agent.",
		Rows:         toolConversationRows(),
	})

	// System prefix breakpoint: with no addendum the system message is one
	// block and it carries the marker.
	if areq.System == nil {
		t.Fatal("expected a system prompt on the wire")
	}
	if got := len(areq.System.ContentBlocks); got != 1 {
		t.Fatalf("system message has %d blocks, want 1 with no addendum", got)
	}
	if areq.System.ContentBlocks[0].CacheControl == nil {
		t.Fatalf("the sole system block must carry a cache breakpoint, got %+v", areq.System)
	}

	// Moving breakpoint: the final message's last block is cached, and no
	// interior message is.
	last := areq.Messages[len(areq.Messages)-1]
	lastBlocks := last.Content.ContentBlocks
	if len(lastBlocks) == 0 || lastBlocks[len(lastBlocks)-1].CacheControl == nil {
		t.Fatalf("final message's last block must carry the moving cache breakpoint, got %+v", last)
	}
	for i := 0; i < len(areq.Messages)-1; i++ {
		for _, b := range areq.Messages[i].Content.ContentBlocks {
			if b.CacheControl != nil {
				t.Fatalf("interior message %d must not carry a cache breakpoint", i)
			}
		}
	}
}

// TestWire_SystemAddendumIsASecondBlockBehindTheBreakpoint is the golden for
// the split system prompt: two text blocks in order, and the marker on the
// FIRST — the shared one. Its position is the assertion. On the last block the
// only system cache entry would sit after this conversation's own bytes and
// the entry every other conversation reads would never be written.
func TestWire_SystemAddendumIsASecondBlockBehindTheBreakpoint(t *testing.T) {
	const (
		shared   = "You are a triage agent."
		perConvo = "<run_context>\nrun root: /work\n</run_context>"
	)
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		SystemPrompt: shared, SystemAddendum: perConvo,
		Rows: toolConversationRows(),
	})

	if areq.System == nil {
		t.Fatal("expected a system prompt on the wire")
	}
	blocks := areq.System.ContentBlocks
	if len(blocks) != 2 {
		t.Fatalf("system message has %d blocks, want 2 (shared, addendum): %+v", len(blocks), areq.System)
	}
	for i, want := range []string{shared, perConvo} {
		if blocks[i].Text == nil {
			t.Fatalf("system block %d carries no text", i)
		}
		if *blocks[i].Text != want {
			t.Fatalf("system block %d is %q, want %q", i, *blocks[i].Text, want)
		}
	}
	if blocks[0].CacheControl == nil {
		t.Fatal("block 1 (the shared prompt) must carry the ephemeral marker")
	}
	if blocks[1].CacheControl != nil {
		t.Fatalf("block 2 (the addendum) must carry no marker of its own, got %+v", blocks[1].CacheControl)
	}
}

// TestWire_NoAddendumKeepsTheSingleSystemBlock pins the empty-addendum case:
// one text block holding exactly the prompt, with the marker on it. A caller
// with nothing per-conversation to say must not pay a second block for it.
func TestWire_NoAddendumKeepsTheSingleSystemBlock(t *testing.T) {
	const shared = "You are a triage agent."
	areq := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		SystemPrompt: shared,
		Rows:         toolConversationRows(),
	})

	if areq.System == nil || areq.System.ContentStr != nil {
		t.Fatalf("system must reach the wire as content blocks, got %+v", areq.System)
	}
	blocks := areq.System.ContentBlocks
	if len(blocks) != 1 {
		t.Fatalf("system message has %d blocks, want exactly 1: %+v", len(blocks), blocks)
	}
	if blocks[0].Text == nil || *blocks[0].Text != shared {
		t.Fatalf("the sole system block is %v, want %q", blocks[0].Text, shared)
	}
	if blocks[0].CacheControl == nil {
		t.Fatal("the sole system block must carry the ephemeral marker")
	}
}

// TestWire_AdaptiveVsBudgetThinkingPerModel pins the effort→thinking shape:
// a 4.x model gets budget_tokens thinking, a Sonnet-5+ model gets adaptive.
func TestWire_AdaptiveVsBudgetThinkingPerModel(t *testing.T) {
	rows := []domain.Message{{ID: 1, Role: "user", Content: "hello"}}

	budget := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		Rows: rows, Effort: "medium", MaxTokens: 8192,
	})
	if budget.Thinking == nil {
		t.Fatal("4.x model with effort must produce a thinking config")
	}
	if budget.Thinking.Type != "enabled" || budget.Thinking.BudgetTokens == nil {
		t.Fatalf("4.x model must use budget_tokens thinking, got type=%q budget=%v", budget.Thinking.Type, budget.Thinking.BudgetTokens)
	}

	adaptive := toAnthropic(t, Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-5",
		Rows: rows, Effort: "medium", MaxTokens: 8192,
	})
	if adaptive.Thinking == nil {
		t.Fatal("Sonnet-5 model with effort must produce a thinking config")
	}
	if adaptive.Thinking.Type != "adaptive" {
		t.Fatalf("Sonnet-5 model must use adaptive thinking, got type=%q", adaptive.Thinking.Type)
	}
	if adaptive.Thinking.BudgetTokens != nil {
		t.Fatal("adaptive thinking must not carry budget_tokens")
	}
}

// TestWire_MarshalSortedDeterministic pins byte-stability: the same assembled
// request marshals to identical bytes across runs, the property prompt caching
// relies on.
func TestWire_MarshalSortedDeterministic(t *testing.T) {
	req := Request{
		Provider: ProviderAnthropic, Model: "claude-sonnet-4-20250514",
		SystemPrompt: "You are a triage agent.",
		Rows:         toolConversationRows(),
	}
	a := toAnthropic(t, req)
	b := toAnthropic(t, req)
	ba, err := schemas.MarshalSorted(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := schemas.MarshalSorted(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ba, bb) {
		t.Fatalf("MarshalSorted must be byte-stable:\n a=%s\n b=%s", ba, bb)
	}
}

func findAssistant(t *testing.T, areq *anthropic.AnthropicMessageRequest, textContains string) anthropic.AnthropicMessage {
	t.Helper()
	for _, m := range areq.Messages {
		if m.Role != anthropic.AnthropicMessageRoleAssistant {
			continue
		}
		for _, b := range m.Content.ContentBlocks {
			if b.Type == anthropic.AnthropicContentBlockTypeText && b.Text != nil && *b.Text == textContains {
				return m
			}
		}
	}
	t.Fatalf("no assistant message containing %q", textContains)
	return anthropic.AnthropicMessage{}
}
