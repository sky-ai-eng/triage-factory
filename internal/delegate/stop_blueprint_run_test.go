package delegate

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The two teardown verbs take ids from different tables, and neither id
// resolves against the other's. These tests pin the pair from both ends: the
// blueprint-run verb tears a run down from the id its caller holds, and each
// verb refuses the other's id outright rather than reporting a teardown it did
// not perform.

// TestStopBlueprintRun_TearsDownTheRunItsIDNames is the firing-revert
// rollback's shape: the caller holds only the blueprint_run id Delegate
// returned — the step conversation under it was minted by the enqueue, not by
// the caller — and it needs every part of that run to stop.
func TestStopBlueprintRun_TearsDownTheRunItsIDNames(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-firing-reverted"
	seedConversation(t, database, conversationID, "sess-fr", "/tmp/wt-fr")
	// A just-enqueued step 0 of an auto-fired blueprint: queued, no creator.
	if _, err := database.Exec(
		`UPDATE conversations SET status = 'queued', trigger_type = 'event', creator_user_id = NULL WHERE id = ?`,
		conversationID); err != nil {
		t.Fatalf("stage queued step: %v", err)
	}
	brID := "seedbpr-" + conversationID

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.StopBlueprintRun(runmode.LocalDefaultOrgID, brID, StopCauseFiringReverted); err != nil {
		t.Fatalf("StopBlueprintRun: %v", err)
	}

	var bpStatus string
	var cancelRequested bool
	if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = ?`, brID).
		Scan(&bpStatus, &cancelRequested); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled — a run left 'running' keeps executing for a firing that was rolled back, and holds its task's one-active-auto-run slot", bpStatus)
	}
	if !cancelRequested {
		t.Error("blueprint_run cancel_requested = false; the signal is what stops the claim gate handing out this blueprint's steps during the teardown")
	}

	var convStatus string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "open" {
		t.Errorf("step conversation status = %q, want open — the teardown parks each step it stops", convStatus)
	}

	notes := stopNotes(t, database, conversationID)
	if len(notes) != 1 {
		t.Fatalf("stop notes = %d, want 1 — each stopped step explains its own ending", len(notes))
	}
	if notes[0].Content != StopCauseFiringReverted.note() {
		t.Errorf("note = %q, want %q", notes[0].Content, StopCauseFiringReverted.note())
	}
}

// TestStopBlueprintRun_NoStepConversations_FinalizesTheRun covers the window
// where a run has been committed but carries no step to stop. Nothing else
// will finalize it — every other path to a blueprint terminal runs off a
// step's — so the verb has to write that terminal itself.
func TestStopBlueprintRun_NoStepConversations_FinalizesTheRun(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-anchor", "sess-anchor", "/tmp/wt-anchor")
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id = 'r-anchor'`).Scan(&taskID); err != nil {
		t.Fatalf("read task id: %v", err)
	}
	// A second blueprint_run on the same task, with no conversations under it.
	brID := seedConversationBlueprint(t, database, "stepless", taskID)

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.StopBlueprintRun(runmode.LocalDefaultOrgID, brID, StopCauseFiringReverted); err != nil {
		t.Fatalf("StopBlueprintRun: %v", err)
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "cancelled" {
		t.Errorf("blueprint_run status = %q, want cancelled — a stepless run with no terminal written is one nothing ever collects", bpStatus)
	}
}

// TestStopBlueprintRun_RefusesAConversationID and its twin below are the
// bug this verb exists for, pinned from both sides: a teardown handed the
// wrong table's id must say so, because the caller that gets a nil error goes
// on to finish a rollback whose first step never happened.
func TestStopBlueprintRun_RefusesAConversationID(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-wrong-id"
	seedConversation(t, database, conversationID, "sess-wrong", "/tmp/wt-wrong")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.StopBlueprintRun(runmode.LocalDefaultOrgID, conversationID, StopCauseFiringReverted)
	if !errors.Is(err, ErrNoActiveBlueprintRun) {
		t.Fatalf("StopBlueprintRun with a conversation id = %v, want ErrNoActiveBlueprintRun", err)
	}
	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, "seedbpr-"+conversationID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running — a refused teardown must not half-cancel the run it could not address", bpStatus)
	}
}

func TestStopAndCancelBlueprint_RefusesABlueprintRunID(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-conv-verb"
	seedConversation(t, database, conversationID, "sess-cv", "/tmp/wt-cv")
	brID := "seedbpr-" + conversationID

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.StopAndCancelBlueprint(runmode.LocalDefaultOrgID, brID, "", StopCauseFiringReverted)
	if !errors.Is(err, ErrNoActiveConversation) {
		t.Fatalf("StopAndCancelBlueprint with a blueprint_run id = %v, want ErrNoActiveConversation", err)
	}
	var convStatus string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "running" {
		t.Errorf("conversation status = %q, want running — the conversation verb reaches nothing when handed a blueprint_run id", convStatus)
	}
}
