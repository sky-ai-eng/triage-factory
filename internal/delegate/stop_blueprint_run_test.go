package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
	if !errors.Is(err, ErrNoSuchBlueprintRun) {
		t.Fatalf("StopBlueprintRun with a conversation id = %v, want ErrNoSuchBlueprintRun", err)
	}
	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, "seedbpr-"+conversationID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running — a refused teardown must not half-cancel the run it could not address", bpStatus)
	}
}

func TestStopConversationAndCancelBlueprint_RefusesABlueprintRunID(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-conv-verb"
	seedConversation(t, database, conversationID, "sess-cv", "/tmp/wt-cv")
	brID := "seedbpr-" + conversationID

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.StopConversationAndCancelBlueprint(runmode.LocalDefaultOrgID, brID, "", StopCauseFiringReverted)
	if !errors.Is(err, ErrNoActiveConversation) {
		t.Fatalf("StopConversationAndCancelBlueprint with a blueprint_run id = %v, want ErrNoActiveConversation", err)
	}
	var convStatus string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "running" {
		t.Errorf("conversation status = %q, want running — the conversation verb reaches nothing when handed a blueprint_run id", convStatus)
	}
}

// cancelSignalFailingBlueprints fails only the durable cancel-signal write.
// Every other read the teardown makes is the real store, so the verb reaches
// the signal exactly as it would in production and fails only there.
type cancelSignalFailingBlueprints struct {
	db.BlueprintStore
}

func (cancelSignalFailingBlueprints) RequestRunCancelSystem(ctx context.Context, orgID, id string) (bool, error) {
	return false, errors.New("boom: the cancel signal did not commit")
}

// TestStopBlueprintRun_CancelSignalFailure_TearsNothingDown pins the ordering
// the teardown depends on. The signal is what stops the claim gate advancing
// the sequence, so without it killing a step does not stop the run — the
// reactor reads that step's terminal, finds no cancel_requested and enqueues
// the next one. Proceeding would trade a live step for its successor and
// return nil, which is worse than the failure it is papering over.
func TestStopBlueprintRun_CancelSignalFailure_TearsNothingDown(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-no-signal"
	seedConversation(t, database, conversationID, "sess-ns", "/tmp/wt-ns")
	brID := "seedbpr-" + conversationID

	stores := testSpawnerStores(database)
	stores.Blueprints = cancelSignalFailingBlueprints{BlueprintStore: stores.Blueprints}
	s := NewSpawner(database, stores, nil, nil, "claude-sonnet-4-6")

	if err := s.StopBlueprintRun(runmode.LocalDefaultOrgID, brID, StopCauseFiringReverted); err == nil {
		t.Fatal("StopBlueprintRun returned nil after the cancel signal failed to commit")
	}

	var convStatus, bpStatus string
	if err := database.QueryRow(`SELECT status FROM conversations WHERE id = ?`, conversationID).Scan(&convStatus); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if convStatus != "running" {
		t.Errorf("step conversation status = %q, want running — a teardown that could not stop the sequence must not kill a step the sequence will replace", convStatus)
	}
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "running" {
		t.Errorf("blueprint_run status = %q, want running", bpStatus)
	}
	if notes := stopNotes(t, database, conversationID); len(notes) != 0 {
		t.Errorf("stop notes = %d, want 0 — nothing was stopped, so the transcript must not say so", len(notes))
	}
}

// TestStopBlueprintRun_ConcludedRunIsItsOwnAnswer keeps the two "nothing to
// tear down" outcomes apart. A run that concluded on its own leaves nothing
// live to orphan, and a run that isn't there at all is an anomaly for a caller
// holding an id it just watched commit — so a caller unwinding its own side
// effects can report what actually happened instead of picking one and being
// wrong half the time.
func TestStopBlueprintRun_ConcludedRunIsItsOwnAnswer(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-concluded"
	seedConversation(t, database, conversationID, "sess-con", "/tmp/wt-con")
	brID := "seedbpr-" + conversationID
	settleConversationBlueprint(t, database, conversationID, "completed")

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	err := s.StopBlueprintRun(runmode.LocalDefaultOrgID, brID, StopCauseFiringReverted)
	if !errors.Is(err, ErrBlueprintRunConcluded) {
		t.Fatalf("StopBlueprintRun on a concluded run = %v, want ErrBlueprintRunConcluded", err)
	}
	if errors.Is(err, ErrNoSuchBlueprintRun) {
		t.Error("a concluded run answered ErrNoSuchBlueprintRun; the run is right there, it is just finished")
	}

	var bpStatus string
	if err := database.QueryRow(`SELECT status FROM blueprint_runs WHERE id = ?`, brID).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint_run: %v", err)
	}
	if bpStatus != "completed" {
		t.Errorf("blueprint_run status = %q, want completed — a teardown must not rewrite a terminal it arrived too late for", bpStatus)
	}
}
