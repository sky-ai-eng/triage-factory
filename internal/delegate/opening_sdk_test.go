package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/inference"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// openingTestMemories is the two-memory handoff most of these cases open with.
func openingTestMemories() []domain.TaskMemory {
	return []domain.TaskMemory{
		{ID: "mem-older", Content: "what the first conversation tried"},
		{ID: "mem-newer", Content: "what the second conversation tried"},
	}
}

const openingTestTaskContext = "<task_context>\nPull request owner/repo#7\n</task_context>"

func blockTexts(blocks []agentproc.ContentBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Text)
	}
	return out
}

// TestOpeningTurnBlocks_MintsTheRowsAndSendsExactlyThem is the parity rule at
// the first turn: the SDK launch mints the same rows the native launch does,
// and what it puts on the wire is those rows through the one assembler — not a
// second composition that can drift from them.
func TestOpeningTurnBlocks_MintsTheRowsAndSendsExactlyThem(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-sdk-open", "", "/tmp/wt-sdk-open")
	claimID := markEngaged(t, database, "r-sdk-open")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-open", claimID, "event", runmode.LocalDefaultUserID)

	blocks, err := s.openingTurnBlocks(context.Background(), sink, runmode.LocalDefaultOrgID, "r-sdk-open",
		runmode.LocalDefaultUserID, openingTestMemories(), openingTestTaskContext)
	if err != nil {
		t.Fatalf("openingTurnBlocks: %v", err)
	}

	rows := allRows(t, s, "r-sdk-open")
	want := []struct{ subtype, content string }{
		{domain.MessageSubtypeInjectionMemory, "what the first conversation tried"},
		{domain.MessageSubtypeInjectionMemory, "what the second conversation tried"},
		{domain.MessageSubtypeInjectionTaskContext, openingTestTaskContext},
	}
	if len(rows) != len(want) {
		t.Fatalf("opening rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i].Role != "user" || rows[i].Subtype != w.subtype || rows[i].Content != w.content {
			t.Errorf("row %d = {role %q subtype %q content %q}, want a user row {%q, %q}",
				i, rows[i].Role, rows[i].Subtype, rows[i].Content, w.subtype, w.content)
		}
	}

	// The blocks are the rows through RowsToMessages and nothing else. Asserted
	// against the assembler rather than against a literal so a change to the
	// envelope or the packing moves both wires at once, which is the whole
	// reason one function builds them.
	assembled, err := inference.RowsToMessages(rows, inference.AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatalf("assemble the rows independently: %v", err)
	}
	if len(assembled) != 1 {
		t.Fatalf("the opening rows assembled to %d messages, want one", len(assembled))
	}
	var wantTexts []string
	for _, b := range assembled[0].Content.ContentBlocks {
		wantTexts = append(wantTexts, *b.Text)
	}
	if strings.Join(blockTexts(blocks), "\x00") != strings.Join(wantTexts, "\x00") {
		t.Errorf("the sent blocks are not RowsToMessages over the minted rows;\ngot  %q\nwant %q", blockTexts(blocks), wantTexts)
	}
	for i, b := range blocks {
		if b.Type != "text" {
			t.Errorf("block %d is %q, want text", i, b.Type)
		}
	}
}

// TestOpeningTurnBlocks_BracketsTheMemoriesAndEndsWithTheTaskContext pins the
// shape the model actually reads: the memory run inside its envelope, then the
// task context, as separate blocks rather than one joined string. The
// boundaries are the point — a joined opening would make the two runtimes'
// first turns equal only in their characters.
func TestOpeningTurnBlocks_BracketsTheMemoriesAndEndsWithTheTaskContext(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-sdk-envelope", "", "/tmp/wt-sdk-envelope")
	claimID := markEngaged(t, database, "r-sdk-envelope")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-envelope", claimID, "event", runmode.LocalDefaultUserID)

	blocks, err := s.openingTurnBlocks(context.Background(), sink, runmode.LocalDefaultOrgID, "r-sdk-envelope",
		runmode.LocalDefaultUserID, openingTestMemories(), openingTestTaskContext)
	if err != nil {
		t.Fatalf("openingTurnBlocks: %v", err)
	}

	want := []string{
		domain.MemoryEnvelopeOpen(2),
		"what the first conversation tried",
		"what the second conversation tried",
		domain.MemoryEnvelopeClose,
		openingTestTaskContext,
	}
	if got := blockTexts(blocks); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("opening blocks =\n  %q\nwant\n  %q", got, want)
	}
}

// TestOpeningTurnBlocks_TaskContextAloneWithNoPriorMemories is the task's first
// conversation: one block, no envelope, and still a block array rather than a
// bare string — the send verb is the same one either way.
func TestOpeningTurnBlocks_TaskContextAloneWithNoPriorMemories(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-sdk-first", "", "/tmp/wt-sdk-first")
	claimID := markEngaged(t, database, "r-sdk-first")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-first", claimID, "event", runmode.LocalDefaultUserID)

	blocks, err := s.openingTurnBlocks(context.Background(), sink, runmode.LocalDefaultOrgID, "r-sdk-first",
		runmode.LocalDefaultUserID, nil, openingTestTaskContext)
	if err != nil {
		t.Fatalf("openingTurnBlocks: %v", err)
	}
	if len(blocks) != 1 || blocks[0].Text != openingTestTaskContext {
		t.Errorf("opening blocks = %q, want the task context alone", blockTexts(blocks))
	}
}

// TestOpeningTurnBlocks_EveryRowIsDelivered is the hazard this leaf carries:
// on the SDK an undelivered user row IS the resume queue, so an opening written
// there would route the next claim down the resume path and be replayed as a
// follow-up the person never typed.
//
// Asserted through the queue's own reader rather than by inspecting the column,
// because that reader is what would misfire.
func TestOpeningTurnBlocks_EveryRowIsDelivered(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-sdk-delivered", "", "/tmp/wt-sdk-delivered")
	claimID := markEngaged(t, database, "r-sdk-delivered")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-delivered", claimID, "event", runmode.LocalDefaultUserID)

	if _, err := s.openingTurnBlocks(context.Background(), sink, runmode.LocalDefaultOrgID, "r-sdk-delivered",
		runmode.LocalDefaultUserID, openingTestMemories(), openingTestTaskContext); err != nil {
		t.Fatalf("openingTurnBlocks: %v", err)
	}

	if got := pendingRows(t, s, "r-sdk-delivered"); len(got) != 0 {
		t.Errorf("undelivered rows = %d, want 0; the opening must not sit in the pending-input queue: %+v", len(got), got)
	}
	msg, _, ok, err := sqlitestore.New(database).ConversationPendingInput.Peek(
		context.Background(), runmode.LocalDefaultOrgID, "r-sdk-delivered")
	if err != nil {
		t.Fatalf("peek the resume queue: %v", err)
	}
	if ok {
		t.Errorf("the resume queue reports pending input %q; a re-claim would replay the opening as a follow-up", msg)
	}
}

// TestOpeningTurnBlocks_SessionReclaimMintsNothingAndResendsTheSameOpening is
// the crash re-claim with a surviving session: the SDK process is new even when
// the session it resumes is not, so it is told what its predecessor was told —
// from the rows already on the transcript, without minting a second opening.
func TestOpeningTurnBlocks_SessionReclaimMintsNothingAndResendsTheSameOpening(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-sdk-reclaim", "", "/tmp/wt-sdk-reclaim")
	claimID := markEngaged(t, database, "r-sdk-reclaim")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	sink := newConversationSink(s, runmode.LocalDefaultOrgID, "r-sdk-reclaim", claimID, "event", runmode.LocalDefaultUserID)
	ctx := context.Background()

	first, err := s.openingTurnBlocks(ctx, sink, runmode.LocalDefaultOrgID, "r-sdk-reclaim",
		runmode.LocalDefaultUserID, openingTestMemories(), openingTestTaskContext)
	if err != nil {
		t.Fatalf("first openingTurnBlocks: %v", err)
	}
	before := len(allRows(t, s, "r-sdk-reclaim"))

	// The engagement dies and a successor re-claims it. The agent it starts has
	// already produced a turn, and a person typed while it was down.
	if _, err := s.conversations.InsertMessageForClaimSystem(ctx, runmode.LocalDefaultOrgID, claimID, &domain.Message{
		ConversationID: "r-sdk-reclaim", Role: "assistant", Content: "on it",
	}); err != nil {
		t.Fatalf("record the agent's turn: %v", err)
	}
	if _, err := s.conversations.InsertMessageSystem(ctx, runmode.LocalDefaultOrgID,
		pendingUserInput("r-sdk-reclaim", runmode.LocalDefaultUserID, "and update the README")); err != nil {
		t.Fatalf("queue the follow-up: %v", err)
	}

	second, err := s.openingTurnBlocks(ctx, sink, runmode.LocalDefaultOrgID, "r-sdk-reclaim",
		runmode.LocalDefaultUserID, openingTestMemories(), openingTestTaskContext)
	if err != nil {
		t.Fatalf("second openingTurnBlocks: %v", err)
	}
	if got := len(allRows(t, s, "r-sdk-reclaim")); got != before+2 {
		t.Errorf("rows after a re-claim = %d, want %d — the opening must not be minted twice", got, before+2)
	}
	if strings.Join(blockTexts(second), "\x00") != strings.Join(blockTexts(first), "\x00") {
		t.Errorf("a re-claim sent a different opening;\ngot  %q\nwant %q", blockTexts(second), blockTexts(first))
	}
}

// TestOpeningContentBlocks_RefusesWhatTheAgentCannotBeSent covers the wiring
// errors the launch fails on rather than starting an agent against a truncated
// opening: a non-text block, and rows that do not assemble to one user turn.
func TestOpeningContentBlocks_RefusesWhatTheAgentCannotBeSent(t *testing.T) {
	cases := []struct {
		name string
		rows []domain.Message
	}{
		{
			name: "a non-text block",
			rows: []domain.Message{{
				ID: 1, Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext,
				Content:       openingTestTaskContext,
				ContentBlocks: []domain.ContentBlock{{Type: "image_url", ImageURL: &domain.ContentImageURL{URL: "https://example.com/x.png"}}},
			}},
		},
		{
			name: "an assistant row among the opening",
			rows: []domain.Message{
				{ID: 1, Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext, Content: openingTestTaskContext},
				{ID: 2, Role: "assistant", Content: "on it"},
			},
		},
		{
			name: "no rows at all",
			rows: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := openingContentBlocks(tc.rows); err == nil {
				t.Errorf("openingContentBlocks accepted %s and returned %q", tc.name, blockTexts(got))
			}
		})
	}
}

// TestOpeningRowsOf_PicksTheOpeningOutOfATranscript is what the re-claim rests
// on: the opening is selected by subtype, so a transcript the control plane
// wrote to in between still yields exactly the rows the conversation opened
// with.
func TestOpeningRowsOf_PicksTheOpeningOutOfATranscript(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Subtype: domain.MessageSubtypeInjectionMemory, Content: "a prior memory"},
		{ID: 2, Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext, Content: openingTestTaskContext},
		{ID: 3, Role: "assistant", Content: "on it"},
		{ID: 4, Role: "user", Content: "and update the README"},
		{ID: 5, Role: "user", Subtype: domain.MessageSubtypeInjectionExecutorChanged, Content: "you moved hosts"},
	}
	got := openingRowsOf(rows)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Errorf("openingRowsOf = %+v, want the memory row and the task-context row", got)
	}
}

// TestOpeningContentBlocks_IsTheAssemblerVerbatim is the no-second-composition
// rule stated on its own: whatever RowsToMessages produces is what reaches the
// wire, block for block.
func TestOpeningContentBlocks_IsTheAssemblerVerbatim(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Subtype: domain.MessageSubtypeInjectionMemory, Content: "a prior memory"},
		{ID: 2, Role: "user", Subtype: domain.MessageSubtypeInjectionTaskContext, Content: openingTestTaskContext},
	}
	blocks, err := openingContentBlocks(rows)
	if err != nil {
		t.Fatalf("openingContentBlocks: %v", err)
	}
	assembled, err := inference.RowsToMessages(rows, inference.AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatalf("RowsToMessages: %v", err)
	}
	if assembled[0].Role != schemas.ChatMessageRoleUser {
		t.Fatalf("the opening assembled to a %s message", assembled[0].Role)
	}
	if len(blocks) != len(assembled[0].Content.ContentBlocks) {
		t.Fatalf("blocks = %d, assembled blocks = %d", len(blocks), len(assembled[0].Content.ContentBlocks))
	}
	for i, b := range assembled[0].Content.ContentBlocks {
		if blocks[i].Text != *b.Text {
			t.Errorf("block %d = %q, want %q", i, blocks[i].Text, *b.Text)
		}
	}
}
