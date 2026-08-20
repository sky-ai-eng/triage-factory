package routing

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Closing a task stops what was running on it; it does not padlock what
// finished. The stop is two halves with different durability: the INTENT
// (cancel_requested on the blueprints behind the task's then-active runs)
// commits with the close, and the KILL that acts on it is best-effort
// afterwards. These tests pin both sides of that line and, just as
// importantly, its edges — the finished blueprint a user may still resume, and
// the replayed close that must not reach back to touch it.

// stoppingSpawner records every StopConversationAndCancelBlueprint call and
// can be made to fail them all, standing in for the kill half failing after
// the close committed — the exact window this ticket exists to make
// survivable.
type stoppingSpawner struct {
	stubDelegator
	mu      sync.Mutex
	stopped []string
	fail    bool
}

func (s *stoppingSpawner) StopConversationAndCancelBlueprint(orgID, conversationID, userID string, cause delegate.StopCause) error {
	s.mu.Lock()
	s.stopped = append(s.stopped, conversationID)
	s.mu.Unlock()
	if s.fail {
		return errors.New("boom: the executor holding this run is unreachable")
	}
	return nil
}

func (s *stoppingSpawner) stoppedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stopped...)
}

// seedRunOnTask stages one blueprint_run in blueprintStatus with its step-0
// conversation on the task, in the given STORED conversation status ("" =
// SQL NULL, the mid-flight state). This is the shape a fired trigger leaves
// behind; the router's own delegation path is not the thing under test here.
func seedRunOnTask(t *testing.T, database *sql.DB, taskID, blueprintStatus, convStatus string) (blueprintRunID, convID string) {
	t.Helper()
	promptID := uuid.New().String()
	createTestPrompt(t, database, domain.Prompt{ID: promptID, Name: "Close Intent", Body: "x", Source: "user"})
	blueprintID := insertBlueprintForTeam(t, database, uuid.New().String(), promptID, runmode.LocalDefaultTeamID)

	stepPlan, err := domain.MarshalStepPlan([]domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: promptID, PromptName: "Close Intent", PromptBody: "x", Source: "user"},
	})
	if err != nil {
		t.Fatalf("marshal step plan: %v", err)
	}
	blueprintRunID = uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id,
		                            status, current_step_index, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', ?, ?, 0, ?, ?)
	`, blueprintRunID, blueprintID, taskID, runmode.LocalDefaultUserID, blueprintStatus,
		"/tmp/wt-"+blueprintRunID, stepPlan); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}

	convID = uuid.New().String()
	var status any
	if convStatus != "" {
		status = convStatus
	}
	if _, err := database.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model, trigger_type,
		                           team_id, visibility, creator_user_id, origin,
		                           blueprint_run_id, blueprint_step_index)
		VALUES (?, ?, ?, ?, 'm', 'manual', ?, 'team', ?, 'blueprint', ?, 0)
	`, convID, taskID, promptID, status, runmode.LocalDefaultTeamID,
		runmode.LocalDefaultUserID, blueprintRunID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return blueprintRunID, convID
}

func cancelRequested(t *testing.T, database *sql.DB, blueprintRunID string) bool {
	t.Helper()
	var flag int
	if err := database.QueryRow(`SELECT cancel_requested FROM blueprint_runs WHERE id = ?`, blueprintRunID).Scan(&flag); err != nil {
		t.Fatalf("read cancel_requested: %v", err)
	}
	return flag != 0
}

// claimNext asks the real dispatcher scan whether anything is drivable. It is
// the question that matters for a stamped run: the gate, not the flag, is what
// keeps a run nothing killed from being picked back up.
func claimNext(t *testing.T, database *sql.DB) *domain.Conversation {
	t.Helper()
	conv, err := sqlitestore.New(database).ConversationQueue.ClaimNextConversation(context.Background(), "test-executor", 1, db.ClaimPlacement{})
	if err != nil {
		t.Fatalf("ClaimNextConversation: %v", err)
	}
	return conv
}

// seedCIFailedTaskOnEntity drains a ci_check_failed event so the entity ends
// up with exactly one live task, and returns it — the task a merge will close.
func seedCIFailedTaskOnEntity(t *testing.T, r *Router, database *sql.DB, sourceID string) (entityID, taskID string) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", sourceID, "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the ci event: %v", err)
	}
	live, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("setup: active tasks = %d (err %v), want 1", len(live), err)
	}
	return entity.ID, live[0].ID
}

// TestCloseCancelIntent_KillFailureStillLeavesTheRunCalledOff is the headline.
// The kill fails after the close commits — the window that used to be
// retry-unrepairable, since a replay finds no active task and never walks back
// to the run. The intent rode the close transaction, so the run is called off
// anyway: the claim gate refuses it, which is what lets the reaper's cancel arm
// finish the job once its executor is gone.
func TestCloseCancelIntent_KillFailureStillLeavesTheRunCalledOff(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{fail: true}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#kill-fails")
	brID, convID := seedRunOnTask(t, database, taskID, "running", "")

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	// The kill was attempted and failed — the premise of the test.
	if got := sp.stoppedIDs(); len(got) != 1 || got[0] != convID {
		t.Fatalf("stop calls = %v, want exactly [%s]", got, convID)
	}
	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Errorf("active tasks = %d, want 0 — the merge closes the task regardless of the kill", n)
	}
	if !cancelRequested(t, database, brID) {
		t.Fatal("cancel_requested = false; a failed kill has forgotten a run the system meant to stop")
	}
	if conv := claimNext(t, database); conv != nil {
		t.Errorf("ClaimNextConversation returned %s; a called-off blueprint must not be driven again", conv.ID)
	}
}

// TestCloseCancelIntent_FinishedBlueprintStaysResumable is the negative that
// bounds the stamp. A blueprint that finished BEFORE its task closed is not
// this close's to call off: a follow-up may resume its final step afterwards,
// which is a legitimate, user-intended state. Stamping it would refuse that
// resume forever, and nothing about the row would say why.
func TestCloseCancelIntent_FinishedBlueprintStaysResumable(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#finished-blueprint")
	brID, convID := seedRunOnTask(t, database, taskID, "completed", "completed")

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Fatalf("active tasks = %d, want 0", n)
	}
	if got := sp.stoppedIDs(); len(got) != 0 {
		t.Errorf("stop calls = %v, want none — a terminal conversation has nothing to stop", got)
	}
	if cancelRequested(t, database, brID) {
		t.Fatal("cancel_requested = true on a blueprint that finished before the close; the post-close resume is now refused forever")
	}

	// The follow-up: the user resumes the final step under the closed task.
	flipped, err := sqlitestore.New(database).Conversations.MarkQueuedForResume(t.Context(), runmode.LocalDefaultOrgID, convID)
	if err != nil || !flipped {
		t.Fatalf("MarkQueuedForResume = (%v, %v), want (true, nil)", flipped, err)
	}
	conv := claimNext(t, database)
	if conv == nil || conv.ID != convID {
		t.Fatalf("ClaimNextConversation = %v, want the resumed run %s — a closed task must not block a resume", conv, convID)
	}
}

// TestCloseCancelIntent_ReplayedCloseStampsNothing pins the replay. The close
// phase is a routing obligation, so a failed pass replays the whole event; the
// tasks it already closed are terminal by then. That second pass must be inert
// toward runs — the one live under the already-closed task may be exactly the
// resumed final step the test above defends, and the replay has no way to tell.
func TestCloseCancelIntent_ReplayedCloseStampsNothing(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#replayed-close", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailedCheck(t, database, entity.ID, "build")
	enqueueCIFailedCheck(t, database, entity.ID, "lint")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the ci events: %v", err)
	}
	live, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(live) != 2 {
		t.Fatalf("setup: active tasks = %d (err %v), want 2", len(live), err)
	}
	doomed, closedFirst := live[0].ID, live[1].ID

	// One task's close fails, so the entity stays active and the event
	// requeues — the close phase's own obligation retry, which is the real
	// path a replay takes rather than a re-enqueue staged by hand.
	r.tasks = &taskCloseOutageStore{TaskStore: testTaskStore(database), failTaskID: doomed, o: &outage{remaining: 1}}
	enqueueMerged(t, database, entity.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := sp.stoppedIDs(); len(got) != 0 {
		t.Fatalf("stop calls on the first pass = %v, want none — neither task had a run", got)
	}

	// Between the passes, a run goes active under the task that already
	// closed. This is the resumed-after-close shape; the replay must leave it
	// entirely alone.
	brID, _ := seedRunOnTask(t, database, closedFirst, "running", "")

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue on the replay: %v", err)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 0 {
		t.Fatalf("active tasks after the replay = %d, want 0", n)
	}
	if cancelRequested(t, database, brID) {
		t.Error("cancel_requested = true; a replayed close reached back and called off a run started after it")
	}
	if got := sp.stoppedIDs(); len(got) != 0 {
		t.Errorf("stop calls = %v, want none — a replay must not re-stop, nor write a second stop note", got)
	}
	if n := closeAuditCount(t, database, closedFirst); n != 1 {
		t.Errorf("close-audit rows on the already-closed task = %d, want 1", n)
	}
}
