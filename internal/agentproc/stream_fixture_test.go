package agentproc

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// fixtureStream is a recorded-shape NDJSON transcript (one JSON object per
// line) covering the full turn this ticket fixes fidelity for: a thinking
// block with a signature, a single text block, two parallel tool_use calls,
// their two parallel tool_result blocks (one plain text, one mixing text and
// an image), a second assistant turn, and the terminal result. It exists so
// a parser regression on any of these shapes fails loudly instead of only
// tripping a narrower unit test.
var fixtureStream = []string{
	`{"type":"system","subtype":"init","session_id":"sess-fixture"}`,
	`{"type":"assistant","message":{"id":"m1","model":"sonnet","content":[{"type":"thinking","thinking":"I should check both files before answering.","signature":"sig-1"}]}}`,
	`{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"I'll check both files."}]}}`,
	`{"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/a.txt"}},{"type":"tool_use","id":"call-2","name":"Read","input":{"file_path":"/b.png"}}],"usage":{"input_tokens":100,"output_tokens":20}}}`,
	`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"file a contents"},{"type":"tool_result","tool_use_id":"call-2","content":[{"type":"text","text":"screenshot of b"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}],"is_error":false}]}}`,
	`{"type":"assistant","message":{"id":"m2","model":"sonnet","stop_reason":"end_turn","content":[{"type":"text","text":"Done."}]}}`,
	`{"type":"result","subtype":"success","is_error":false,"duration_ms":42,"num_turns":2,"total_cost_usd":0.05,"stop_reason":"end_turn","result":"Done."}`,
}

// TestParseLine_FixtureStream feeds fixtureStream through ParseLine end to
// end and pins the exact resulting row shape: reasoning riding the assistant
// row (not a separate thinking row), a single concatenated text block with
// no spurious ContentBlocks promotion, both tool_use calls preserved, both
// parallel tool_results surfacing as separate rows with the image captured
// on ContentBlocks, and the terminal Result populated.
func TestParseLine_FixtureStream(t *testing.T) {
	s := NewStreamState()
	var all []*domain.Message
	var result *Result
	for _, line := range fixtureStream {
		msgs, res := s.ParseLine([]byte(line), "run-1")
		all = append(all, msgs...)
		if res != nil {
			result = res
		}
	}

	if got := s.SessionID(); got != "sess-fixture" {
		t.Fatalf("SessionID = %q, want sess-fixture", got)
	}

	if len(all) != 4 {
		t.Fatalf("expected 4 emitted messages, got %d: %+v", len(all), all)
	}

	// Message 0: the first assistant turn — thinking rode Reasoning, the
	// single text block is flat Content with no ContentBlocks promotion,
	// and both tool_use calls landed on ToolCalls in order.
	asst1 := all[0]
	if asst1.Role != "assistant" || asst1.ConversationID != "run-1" || asst1.Subtype != "" {
		t.Errorf("asst1 shape wrong: %+v", asst1)
	}
	if asst1.Content != "I'll check both files." {
		t.Errorf("asst1.Content = %q", asst1.Content)
	}
	if asst1.ContentBlocks != nil {
		t.Errorf("asst1.ContentBlocks should stay nil for a single text block, got %+v", asst1.ContentBlocks)
	}
	if len(asst1.Reasoning) != 1 || asst1.Reasoning[0].Index != 0 ||
		asst1.Reasoning[0].Text != "I should check both files before answering." ||
		asst1.Reasoning[0].Signature != "sig-1" {
		t.Errorf("asst1.Reasoning wrong: %+v", asst1.Reasoning)
	}
	if len(asst1.ToolCalls) != 2 ||
		asst1.ToolCalls[0].ID != "call-1" || asst1.ToolCalls[0].Name != "Read" ||
		asst1.ToolCalls[1].ID != "call-2" || asst1.ToolCalls[1].Name != "Read" {
		t.Errorf("asst1.ToolCalls wrong: %+v", asst1.ToolCalls)
	}
	if asst1.InputTokens == nil || *asst1.InputTokens != 100 {
		t.Errorf("asst1.InputTokens wrong: %+v", asst1.InputTokens)
	}

	// Message 1: tool_result for call-1 — plain string content, no blocks.
	tool1 := all[1]
	if tool1.Role != "tool" || tool1.ToolCallID != "call-1" || tool1.Content != "file a contents" {
		t.Errorf("tool1 wrong: %+v", tool1)
	}
	if tool1.IsError || tool1.ContentBlocks != nil {
		t.Errorf("tool1 should have no error and no content blocks: %+v", tool1)
	}

	// Message 2: tool_result for call-2 — text flattens to Content, the
	// image block survives on ContentBlocks as a data: URI.
	tool2 := all[2]
	if tool2.Role != "tool" || tool2.ToolCallID != "call-2" || tool2.Content != "screenshot of b" {
		t.Errorf("tool2 wrong: %+v", tool2)
	}
	if tool2.IsError {
		t.Errorf("tool2 should not be an error: %+v", tool2)
	}
	if len(tool2.ContentBlocks) != 1 {
		t.Fatalf("tool2.ContentBlocks wrong: %+v", tool2.ContentBlocks)
	}
	img := tool2.ContentBlocks[0]
	if img.Type != domain.ContentBlockImage || img.ImageURL == nil || img.ImageURL.URL != "data:image/png;base64,QUJD" {
		t.Errorf("tool2 image block wrong: %+v", img)
	}

	// Message 3: the second assistant turn — plain text, no reasoning, no
	// tool calls, no content blocks.
	asst2 := all[3]
	if asst2.Role != "assistant" || asst2.Subtype != "" || asst2.Content != "Done." {
		t.Errorf("asst2 wrong: %+v", asst2)
	}
	if asst2.Reasoning != nil || asst2.ContentBlocks != nil || asst2.ToolCalls != nil {
		t.Errorf("asst2 should carry no reasoning/content_blocks/tool_calls: %+v", asst2)
	}

	// None of the four rows regress to the legacy separate thinking-row
	// shape.
	for i, m := range all {
		if m.Subtype == "thinking" {
			t.Fatalf("message %d regressed to a separate thinking row: %+v", i, m)
		}
	}

	if result == nil {
		t.Fatal("expected a terminal Result")
	}
	if result.IsError || result.DurationMs != 42 || result.NumTurns != 2 || result.CostUSD != 0.05 ||
		result.Result != "Done." {
		t.Errorf("Result wrong: %+v", result)
	}

	// The model's stop reason rides the assistant rows, one per turn — the
	// tool-calling turn and the one that finished, each with its own.
	if asst1.StopReason != "tool_use" || asst2.StopReason != "end_turn" {
		t.Errorf("per-turn stop reasons = (%q, %q), want (tool_use, end_turn)", asst1.StopReason, asst2.StopReason)
	}
}
