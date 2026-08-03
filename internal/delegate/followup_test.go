package delegate

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// finishBlueprint settles a seeded blueprint_run on the step its conversation
// ran — the shape a blueprint leaves behind when its plan completes. Only
// AdvanceStep writes current_step_index and no terminal write touches it, so a
// blueprint that stopped at step N really does leave the column at N; the
// fixture writes both directly because the guarded store method refuses a
// non-running row.
func finishBlueprint(t *testing.T, database *sql.DB, blueprintRunID, status string, currentStep int) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE blueprint_runs SET status = ?, current_step_index = ? WHERE id = ?`,
		status, currentStep, blueprintRunID,
	); err != nil {
		t.Fatalf("finish blueprint: %v", err)
	}
}

// blueprintState reads back the two columns locked decision 2 says a follow-up
// must never move.
func blueprintState(t *testing.T, database *sql.DB, blueprintRunID string) (string, int) {
	t.Helper()
	var status string
	var step int
	if err := database.QueryRow(
		`SELECT status, current_step_index FROM blueprint_runs WHERE id = ?`, blueprintRunID,
	).Scan(&status, &step); err != nil {
		t.Fatalf("read blueprint state: %v", err)
	}
	return status, step
}

func taskStatus(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	return status
}

// TestFollowUpOnFinishedBlueprint_IsClaimedAndDriven is the acceptance test for
// the trap: a row that flips to claimable but is never CLAIMED fails silently
// and permanently, and it would pass any assertion that only checked the resume
// was accepted. So this claims for real, through the same ClaimNextRun the
// dispatcher calls, and then drives the claim.
//
// The failure it guards is two-sided. Before the claim gate widened, the claim
// itself returned nothing. After it widened but before the dispatch guard did,
// the claim succeeded and dispatchClaimedRun immediately parked the
// conversation as "Blueprint cancelled by user" — a worse outcome than the
// refusal, because it looks like it worked.
//
// The delivery here fails fast (no session transcript in a bare temp worktree)
// and lands the conversation terminal. That is the assertion: reaching a
// terminal at all means the claim went down the resume path rather than the
// raced-cancel park.
func TestFollowUpOnFinishedBlueprint_IsClaimedAndDriven(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-followup", "sess-followup", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-followup'`); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	bpr := blueprintRunIDForRun(t, database, "r-followup")
	finishBlueprint(t, database, bpr, "completed", 0)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-followup", runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage on a finished blueprint's final conversation: %v", err)
	}

	// Fatals if nothing is claimable — which is exactly the silent failure.
	claimAndDispatch(t, s, database)

	st := storedStatus(t, database, "r-followup")
	if st == "" || st == "open" {
		t.Fatalf("stored status = %q, want a terminal — the claim never reached delivery", st)
	}
	var stopReason sql.NullString
	if err := database.QueryRow(`SELECT stop_reason FROM conversations WHERE id='r-followup'`).Scan(&stopReason); err != nil {
		t.Fatalf("read stop_reason: %v", err)
	}
	if stopReason.String == "user_cancelled" {
		t.Error("the follow-up claim was parked as a cancelled blueprint step; it must route to the resume path")
	}
	if _, _, ok, err := s.pendingInput.Consume(context.Background(), runmode.LocalDefaultOrgID, "r-followup"); err != nil || ok {
		t.Errorf("pending input survived the dispatch: ok=%v err=%v", ok, err)
	}
}

// TestFollowUpOnFinishedBlueprint_LeavesTheBlueprintAndTaskAlone pins locked
// decision 2 (finished machinery never restarts) and locked decision 1 (done
// tasks never leave the done column) across a whole follow-up. Both guards
// exist already and nothing else stops someone removing one.
func TestFollowUpOnFinishedBlueprint_LeavesTheBlueprintAndTaskAlone(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, database, runID, taskID := setupAdvanceFixture(t, "followup-frozen")
	stampBotClaim(t, database, taskID)
	if _, err := database.Exec(
		`UPDATE conversations SET status='completed', outcome='finish', worktree_path=? WHERE id=?`,
		t.TempDir(), runID,
	); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	if _, err := database.Exec(`UPDATE tasks SET status='done' WHERE id=?`, taskID); err != nil {
		t.Fatalf("close task: %v", err)
	}
	bpr := blueprintRunIDForRun(t, database, runID)
	finishBlueprint(t, database, bpr, "completed", 0)

	wantStatus, wantStep := blueprintState(t, database, bpr)

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, runID, runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	// The resume alone must not move anything.
	if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != wantStatus || gotStep != wantStep {
		t.Errorf("after the resume, blueprint = (%q, %d), want (%q, %d)", gotStatus, gotStep, wantStatus, wantStep)
	}
	if got := taskStatus(t, database, taskID); got != "done" {
		t.Errorf("after the resume, task status = %q, want done", got)
	}

	// Nor must driving it to its own conclusion.
	claimAndDispatch(t, s, database)
	if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != wantStatus || gotStep != wantStep {
		t.Errorf("after the follow-up concluded, blueprint = (%q, %d), want (%q, %d)", gotStatus, gotStep, wantStatus, wantStep)
	}
	if got := taskStatus(t, database, taskID); got != "done" {
		t.Errorf("after the follow-up concluded, task status = %q, want done", got)
	}
}

// TestFollowUpOnFinishedBlueprint_NonFinalStepIsRefused: the snapshot key is
// the blueprint run, one blob every step overwrites, so an earlier step would
// rehydrate the last step's tree. The refusal is at the claim, which is where
// it has to be — the conversation row alone reads perfectly resumable.
func TestFollowUpOnFinishedBlueprint_NonFinalStepIsRefused(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-earlier", "sess-earlier", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-earlier'`); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	bpr := blueprintRunIDForRun(t, database, "r-earlier")
	// The blueprint ran on to step 1; this conversation is step 0.
	finishBlueprint(t, database, bpr, "completed", 1)
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-earlier", runmode.LocalDefaultUserID, "one more thing")
	if err == nil {
		t.Fatal("a non-final step of a finished blueprint accepted a follow-up; nothing would ever claim it")
	}
	if st := storedStatus(t, database, "r-earlier"); st != "completed" {
		t.Errorf("stored status = %q, want completed — a refused wake writes nothing", st)
	}
}

// TestModelForClaim_FollowUpDoesNotInheritTheStepModel pins locked decision 7.
// A blueprint's steps keep the model frozen onto them; a follow-up on the
// finished blueprint does not, because a review blueprint's last step is a
// cheap aggregator and answering someone's first follow-up with it is how they
// decide follow-ups don't work.
func TestModelForClaim_FollowUpDoesNotInheritTheStepModel(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "team-default-model")
	run := domain.Conversation{Model: "cheap-aggregator", TeamID: runmode.LocalDefaultTeamID}
	ctx := context.Background()

	running := &domain.BlueprintRun{Status: domain.BlueprintRunStatusRunning}
	if got := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, running, run); got != "cheap-aggregator" {
		t.Errorf("step of a running blueprint = %q, want the step's own model (no mid-blueprint drift)", got)
	}

	for _, term := range []domain.BlueprintRunStatus{
		domain.BlueprintRunStatusCompleted,
		domain.BlueprintRunStatusAborted,
		domain.BlueprintRunStatusFailed,
	} {
		if got := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: term}, run); got != "team-default-model" {
			t.Errorf("follow-up under a %s blueprint = %q, want the configured default", term, got)
		}
	}

	// A resolver that answers nothing must not blank the model out from under
	// the turn — the step's is a worse answer than the default, never no answer.
	s.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) string { return "" })
	s.model = ""
	if got := s.modelForClaim(ctx, runmode.LocalDefaultOrgID, &domain.BlueprintRun{Status: domain.BlueprintRunStatusCompleted}, run); got != "cheap-aggregator" {
		t.Errorf("unresolvable default = %q, want a fall back to the step's model", got)
	}
}

// TestFollowUpRecordsTaskEvent: the resume moves nothing a person can see, so
// the task_events row is the only trace the closed card keeps of work
// continuing after it closed.
func TestFollowUpRecordsTaskEvent(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-timeline", "sess-timeline", t.TempDir())
	if _, err := database.Exec(`UPDATE conversations SET status='completed', outcome='finish' WHERE id='r-timeline'`); err != nil {
		t.Fatalf("finish conversation: %v", err)
	}
	bpr := blueprintRunIDForRun(t, database, "r-timeline")
	finishBlueprint(t, database, bpr, "completed", 0)
	var taskID string
	if err := database.QueryRow(`SELECT task_id FROM conversations WHERE id='r-timeline'`).Scan(&taskID); err != nil {
		t.Fatalf("read task id: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, "r-timeline", runmode.LocalDefaultUserID, "one more thing"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var kind, metadata string
	if err := database.QueryRow(`
		SELECT te.kind, e.metadata_json
		FROM task_events te JOIN events e ON e.id = te.event_id
		WHERE te.task_id = ? AND e.event_type = ?
	`, taskID, domain.EventSystemConversationResumed).Scan(&kind, &metadata); err != nil {
		t.Fatalf("read the resume task_event: %v", err)
	}
	if kind != "resumed" {
		t.Errorf("task_events.kind = %q, want resumed", kind)
	}
	var meta events.SystemConversationResumedMetadata
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.ConversationID != "r-timeline" || meta.TaskID != taskID || meta.UserID != runmode.LocalDefaultUserID {
		t.Errorf("metadata = %+v, want it to name the conversation, task and actor", meta)
	}
	if meta.BlueprintRunID != bpr || meta.FromStatus != "completed" || meta.FromOutcome != "finish" {
		t.Errorf("metadata = %+v, want the pre-flip rest state and the blueprint it belonged to", meta)
	}
	if got := taskStatus(t, database, taskID); got == "done" {
		t.Error("fixture is not exercising the interesting case; the task was already done")
	}
}

// TestDispatchClaimedRun_FinishedBlueprintWithoutAMessageParks covers the one
// window the widened gate opens: an SDK claim on a finished blueprint whose
// staged message an earlier claim already consumed before dying. The step
// machinery would re-run the final step's mission from the top on concluded
// work, so the claim parks instead and leaves the workspace for the next
// follow-up.
func TestDispatchClaimedRun_FinishedBlueprintWithoutAMessageParks(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-nomsg", "sess-nomsg", t.TempDir())
	bpr := blueprintRunIDForRun(t, database, "r-nomsg")
	finishBlueprint(t, database, bpr, "completed", 0)
	// Mid-flight with no staged message: what a crash between the resume
	// message's consume and the turn's conclusion leaves behind.
	if _, err := database.Exec(`UPDATE conversations SET status=NULL WHERE id='r-nomsg'`); err != nil {
		t.Fatalf("mid-flight: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	claimAndDispatch(t, s, database)

	if st := storedStatus(t, database, "r-nomsg"); st != "open" {
		t.Errorf("stored status = %q, want open — a message-less follow-up claim parks rather than re-running the step", st)
	}
	if gotStatus, gotStep := blueprintState(t, database, bpr); gotStatus != "completed" || gotStep != 0 {
		t.Errorf("blueprint = (%q, %d), want it untouched at (completed, 0)", gotStatus, gotStep)
	}
}
