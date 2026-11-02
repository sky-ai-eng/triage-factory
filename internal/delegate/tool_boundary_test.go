package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestToolBoundary_StagedNotesRideTheNextArmedToolBatchOnce: a note staged
// before the process registered rides its first tool batch; after that a
// batch hands the agent nothing, and reads no rows, until a note is staged at
// the boundary; the next batch then hands over every staged note, marked
// delivered, and the batch after it nothing again.
func TestToolBoundary_StagedNotesRideTheNextArmedToolBatchOnce(t *testing.T) {
	const convID = "run-boundary"
	database := newDelegateTestDB(t)
	seedConversation(t, database, convID, "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	if !s.stageNoteAtToolBoundary(ctx, org, convID, domain.StagedInjectionProducerTaskClosing, "staged while starting", domain.NoteProvenance{}) {
		t.Fatal("the note staged before the process registered was not staged")
	}
	s.registerProc(org, convID, nil)
	if got := s.notesAtToolBatch(org, convID).Text; !strings.Contains(got, "staged while starting") {
		t.Fatalf("the first batch handed over %q, want the note staged while the process started", got)
	}

	if _, ok := s.stageInjection(ctx, org, convID, domain.MessageSubtypeInjectionSystemNote, "pr_note", "a pull request note", domain.NoteProvenance{}); !ok {
		t.Fatal("the unarmed note was not staged")
	}
	if got := s.notesAtToolBatch(org, convID).Text; got != "" {
		t.Fatalf("an unarmed batch handed over %q", got)
	}
	if !s.stageNoteAtToolBoundary(ctx, org, convID, domain.StagedInjectionProducerTaskClosing, "the task is resolved", domain.NoteProvenance{}) {
		t.Fatal("the note was not staged")
	}

	got := s.notesAtToolBatch(org, convID).Text
	for _, want := range []string{"<system-note>", "While you were working", "a pull request note", "the task is resolved"} {
		if !strings.Contains(got, want) {
			t.Errorf("batch handed over %q, missing %q", got, want)
		}
	}
	if left, err := s.stagedInjections.FlushPendingSystem(ctx, org, convID); err != nil || len(left) != 0 {
		t.Errorf("still staged after the batch: %v (err %v)", left, err)
	}
	if again := s.notesAtToolBatch(org, convID).Text; again != "" {
		t.Errorf("the next batch handed over %q, want nothing", again)
	}
}

// TestToolBoundary_NotesTheModelNeverReadRideTheNextBatch: undoing a batch's
// notes, as the reader does when an interrupt gave the batch up first, puts
// them back on the queue and re-arms the boundary, so the next batch hands
// them over again.
func TestToolBoundary_NotesTheModelNeverReadRideTheNextBatch(t *testing.T) {
	const convID = "run-boundary-undo"
	database := newDelegateTestDB(t)
	seedConversation(t, database, convID, "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	s.registerProc(org, convID, nil)

	if !s.stageNoteAtToolBoundary(ctx, org, convID, domain.StagedInjectionProducerTaskClosing, "the task is resolved", domain.NoteProvenance{}) {
		t.Fatal("the note was not staged")
	}
	first := s.notesAtToolBatch(org, convID)
	if !strings.Contains(first.Text, "the task is resolved") || first.Undo == nil {
		t.Fatalf("the batch handed over %+v, want the note with an undo", first)
	}
	first.Undo()
	if again := s.notesAtToolBatch(org, convID).Text; !strings.Contains(again, "the task is resolved") {
		t.Errorf("the batch after the undo handed over %q, want the note again", again)
	}
}
