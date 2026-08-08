package agentloop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
)

// The claim-start repair's placement contract. What these assert is the
// provider's own rule — a tool_use is answered in the message immediately
// after the one that made it — held against transcripts where rows arrived
// between the call and the repair that closes it. Asserting on row order
// alone would miss it: the rows can be in the right sequence in the table and
// still assemble into a request the API rejects.

// assertToolResultsAreAdjacent applies the adjacency rule to a request as the
// model will see it. Every tool_use id an assistant message carries must be
// answered within the run of tool messages that immediately follows it — the
// exact condition behind Anthropic's "tool_use ids were found without
// tool_result blocks immediately after".
func assertToolResultsAreAdjacent(t *testing.T, req inference.Request) {
	t.Helper()
	msgs, err := inference.RowsToMessages(req.Rows, inference.AssemblyOptions{})
	if err != nil {
		t.Fatalf("assemble request: %v", err)
	}
	for i, m := range msgs {
		if m.Role != schemas.ChatMessageRoleAssistant || m.ChatAssistantMessage == nil {
			continue
		}
		unanswered := map[string]struct{}{}
		for _, call := range m.ToolCalls {
			if call.ID != nil && *call.ID != "" {
				unanswered[*call.ID] = struct{}{}
			}
		}
		if len(unanswered) == 0 {
			continue
		}
		for j := i + 1; j < len(msgs) && msgs[j].Role == schemas.ChatMessageRoleTool; j++ {
			if msgs[j].ChatToolMessage == nil || msgs[j].ToolCallID == nil {
				continue
			}
			delete(unanswered, *msgs[j].ToolCallID)
		}
		if len(unanswered) > 0 {
			var ids []string
			for id := range unanswered {
				ids = append(ids, id)
			}
			t.Fatalf("assistant message %d's calls %v are not answered in the messages immediately after it; assembled: %s",
				i, ids, describeAssembly(msgs))
		}
	}
}

// describeAssembly renders an assembled request as a role sequence, so a
// failure names the shape that broke rather than dumping every message.
func describeAssembly(msgs []schemas.ChatMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.ChatAssistantMessage != nil && len(m.ToolCalls) > 0:
			var ids []string
			for _, c := range m.ToolCalls {
				if c.ID != nil {
					ids = append(ids, *c.ID)
				}
			}
			parts = append(parts, fmt.Sprintf("assistant(tool_use:%s)", strings.Join(ids, ",")))
		case m.ChatToolMessage != nil && m.ToolCallID != nil:
			parts = append(parts, fmt.Sprintf("tool(%s)", *m.ToolCallID))
		default:
			parts = append(parts, string(m.Role))
		}
	}
	return strings.Join(parts, " → ")
}

// syntheticResultFor returns the repair's row for a call id, or nil.
func syntheticResultFor(tr *memTranscript, callID string) *domain.Message {
	return tr.find(func(m domain.Message) bool {
		return m.Role == "tool" && m.ToolCallID == callID &&
			strings.Contains(m.Content, "effects may be partially present or absent")
	})
}

// TestRepair_AnchorsSyntheticResultUnderInputThatArrivedAfterTheCall is the
// incident, reproduced: the agent was mid tool call, the user queued a
// follow-up and then stopped the run, and the queued input made the
// conversation immediately re-claimable. Both user rows carry ids above the
// tool_use, so a tail-appended result assembles behind them and the first call
// of the next claim is rejected — deterministically, which fails the
// engagement and takes its workspace with it.
func TestRepair_AnchorsSyntheticResultUnderInputThatArrivedAfterTheCall(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		pendingUser("also update the changelog"),
		domain.Message{
			ConversationID: "conv", Role: "user",
			Subtype: domain.MessageSubtypeStopNote, Content: "This run was stopped by the user.",
		},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}

	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	repaired := syntheticResultFor(tr, "c1")
	if repaired == nil {
		t.Fatal("the interrupted call must get a synthetic result")
	}
	// Strictly between the assistant row that owns the call (id 2) and the
	// follow-up that arrived next (id 3): the answer slots underneath the
	// user's rows without moving them.
	if repaired.Seq == nil || *repaired.Seq <= 2 || *repaired.Seq >= 3 {
		t.Fatalf("synthetic result seq = %v, want a fraction between the owning call (2) and the queued follow-up (3)", repaired.Seq)
	}
	// The user's own rows keep their positions — their order is the record of
	// what was said and when, and the repair has no business rewriting it.
	for _, content := range []string{"also update the changelog", "This run was stopped by the user."} {
		row := tr.find(func(m domain.Message) bool { return m.Content == content })
		if row == nil {
			t.Fatalf("row %q vanished", content)
		}
		if row.Seq != nil {
			t.Errorf("user row %q was re-seqed to %v; only the synthetic result may move", content, *row.Seq)
		}
	}

	if len(p.requests) == 0 {
		t.Fatal("no provider call was made")
	}
	assertToolResultsAreAdjacent(t, p.requests[0])
}

// TestRepair_PartiallyAnsweredBatchSlotsBehindTheRealResult: a batch that got
// one of its two results before the crash keeps that result first. The
// synthetic goes after it and before the input that arrived later, so the
// whole answer block stays contiguous behind the call.
func TestRepair_PartiallyAnsweredBatchSlotsBehindTheRealResult(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{
			{ID: "c1", Name: "bash"}, {ID: "c2", Name: "write"},
		}},
		domain.Message{ConversationID: "conv", Role: "tool", ToolCallID: "c1", Content: "already recorded"},
		pendingUser("also update the changelog"),
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}

	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}

	repaired := syntheticResultFor(tr, "c2")
	if repaired == nil {
		t.Fatal("the unanswered call must get a synthetic result")
	}
	// Between the real result (id 3) and the queued follow-up (id 4).
	if repaired.Seq == nil || *repaired.Seq <= 3 || *repaired.Seq >= 4 {
		t.Fatalf("synthetic result seq = %v, want a fraction between the real result (3) and the follow-up (4)", repaired.Seq)
	}
	if kept := tr.find(func(m domain.Message) bool { return m.ToolCallID == "c1" }); kept == nil || kept.Seq != nil {
		t.Errorf("the real result must keep its position: %+v", kept)
	}
	assertToolResultsAreAdjacent(t, p.requests[0])
}

// TestRepair_CrashShapedTranscriptStillAppends: nothing followed the tool call,
// so the anchored position and the tail are the same row. The result is written
// exactly as it was before placement existed — no seq — which is what keeps
// every already-repaired transcript assembling byte-identically.
func TestRepair_CrashShapedTranscriptStillAppends(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}

	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	repaired := syntheticResultFor(tr, "c1")
	if repaired == nil {
		t.Fatal("the interrupted call must get a synthetic result")
	}
	if repaired.Seq != nil {
		t.Fatalf("crash-shaped repair wrote seq = %v; a tail append must stay a tail append", *repaired.Seq)
	}
	assertToolResultsAreAdjacent(t, p.requests[0])
}

// TestRepair_EachInterruptedTurnAnchorsToItsOwnCall: two crashes in one
// transcript. Both dangling calls are answered, each beneath its own owner —
// the second engagement's call does not drag the first one's answer down to it.
func TestRepair_EachInterruptedTurnAnchorsToItsOwnCall(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		domain.Message{ConversationID: "conv", Role: "user", Content: "keep going"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c2", Name: "write"}}},
		pendingUser("and the changelog"),
	)
	p := &scriptedProvider{turns: []scriptedTurn{{text: "recovered"}}}

	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	first, second := syntheticResultFor(tr, "c1"), syntheticResultFor(tr, "c2")
	if first == nil || second == nil {
		t.Fatalf("both interrupted calls need answers: c1=%v c2=%v", first, second)
	}
	if first.Seq == nil || *first.Seq <= 2 || *first.Seq >= 3 {
		t.Errorf("c1's result seq = %v, want a fraction between its own call (2) and the next row (3)", first.Seq)
	}
	if second.Seq == nil || *second.Seq <= 4 || *second.Seq >= 5 {
		t.Errorf("c2's result seq = %v, want a fraction between its own call (4) and the next row (5)", second.Seq)
	}
	assertToolResultsAreAdjacent(t, p.requests[0])
}

// TestRepair_HealthyTranscriptIsUntouched: the repair is a no-op when every
// call has its result, seq included — a pass that quietly stamped positions on
// a healthy transcript would invalidate a warm cache for nothing.
func TestRepair_HealthyTranscriptIsUntouched(t *testing.T) {
	tr := newMemTranscript(
		domain.Message{ConversationID: "conv", Role: "user", Content: "go"},
		domain.Message{ConversationID: "conv", Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c1", Name: "bash"}}},
		domain.Message{ConversationID: "conv", Role: "tool", ToolCallID: "c1", Content: "ok"},
		pendingUser("one more thing"),
	)
	before := len(tr.snapshot())
	p := &scriptedProvider{turns: []scriptedTurn{{text: "done"}}}

	if got := newTestEngine(tr, p, newScriptedToolHost()).Run(context.Background(), testParams()); got.Kind != ResultConcluded {
		t.Fatalf("disposition = %v, want concluded (err: %v)", got.Kind, got.Err)
	}
	for _, r := range tr.snapshot()[:before] {
		if r.Seq != nil {
			t.Errorf("row %d was re-seqed to %v by a repair that had nothing to repair", r.ID, *r.Seq)
		}
	}
}

// TestPlaceAfterAnswers covers the placement arithmetic itself, including the
// case the loop cannot stage end-to-end: an anchor whose neighbours already
// carry fractional positions from a compaction re-seq. Nothing in the window
// may assume integer seq, so the math divides effective positions rather than
// ids.
func TestPlaceAfterAnswers(t *testing.T) {
	seq := func(v float64) *float64 { return &v }
	calls := func(ids ...string) []domain.ToolCall {
		out := make([]domain.ToolCall, 0, len(ids))
		for _, id := range ids {
			out = append(out, domain.ToolCall{ID: id})
		}
		return out
	}

	t.Run("a batch spreads in call order between its anchor and the next row", func(t *testing.T) {
		rows := []domain.Message{
			{ID: 1, Role: "user"},
			{ID: 2, Role: "assistant", ToolCalls: calls("c1", "c2")},
			{ID: 3, Role: "user"},
		}
		got := placeAfterAnswers(rows, 1, calls("c1", "c2"))
		if len(got) != 2 {
			t.Fatalf("placements = %d, want one per missing call", len(got))
		}
		if got[0].call.ID != "c1" || got[1].call.ID != "c2" {
			t.Fatalf("placements out of call order: %s, %s", got[0].call.ID, got[1].call.ID)
		}
		if !(2 < *got[0].seq && *got[0].seq < *got[1].seq && *got[1].seq < 3) {
			t.Fatalf("seqs = (%v, %v), want ascending strictly within (2, 3)", *got[0].seq, *got[1].seq)
		}
	})

	t.Run("fractional neighbours divide without collision", func(t *testing.T) {
		rows := []domain.Message{
			{ID: 9, Role: "assistant", ToolCalls: calls("c1"), Seq: seq(12.25)},
			{ID: 10, Role: "user", Seq: seq(12.5)},
		}
		got := placeAfterAnswers(rows, 0, calls("c1"))
		if len(got) != 1 || got[0].seq == nil {
			t.Fatalf("placements = %+v, want one positioned result", got)
		}
		if !(12.25 < *got[0].seq && *got[0].seq < 12.5) {
			t.Fatalf("seq = %v, want strictly between the neighbouring fractions", *got[0].seq)
		}
	})

	t.Run("the anchor is the end of the batch's existing answers", func(t *testing.T) {
		rows := []domain.Message{
			{ID: 1, Role: "assistant", ToolCalls: calls("c1", "c2")},
			{ID: 2, Role: "tool", ToolCallID: "c1"},
			{ID: 3, Role: "user"},
		}
		got := placeAfterAnswers(rows, 0, calls("c2"))
		if len(got) != 1 || got[0].seq == nil || !(2 < *got[0].seq && *got[0].seq < 3) {
			t.Fatalf("placement = %+v, want a position after the real result (2) and before the user row (3)", got)
		}
	})

	t.Run("an anchor at the tail appends", func(t *testing.T) {
		rows := []domain.Message{
			{ID: 1, Role: "user"},
			{ID: 2, Role: "assistant", ToolCalls: calls("c1")},
		}
		got := placeAfterAnswers(rows, 1, calls("c1"))
		if len(got) != 1 || got[0].seq != nil {
			t.Fatalf("placement = %+v, want an unpositioned tail append", got)
		}
	})

	t.Run("nothing missing places nothing", func(t *testing.T) {
		rows := []domain.Message{{ID: 1, Role: "assistant"}}
		if got := placeAfterAnswers(rows, 0, nil); got != nil {
			t.Fatalf("placements = %+v, want none", got)
		}
	})
}
