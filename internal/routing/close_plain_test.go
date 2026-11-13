package routing

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A close writes the task's close and nothing else. These tests pin what
// every event close leaves alone: the conversation working on the task, the
// blueprint behind it, and a step parked on a person, each as drivable or
// resumable after the close as it was before. The conversations working when
// the task closes are told, and nothing is stopped.

// workState is the blueprint run and its conversation as stored: what no
// close may change.
type workState struct {
	runStatus          string
	cancelRequested    bool
	conversationStatus string
	stopRequested      bool
	ended              bool
}

func readWorkState(t *testing.T, database *sql.DB, blueprintRunID, convID string) workState {
	t.Helper()
	var st workState
	if err := database.QueryRow(`SELECT status, cancel_requested FROM blueprint_runs WHERE id = ?`,
		blueprintRunID).Scan(&st.runStatus, &st.cancelRequested); err != nil {
		t.Fatalf("read blueprint run %s: %v", blueprintRunID, err)
	}
	if err := database.QueryRow(`
		SELECT COALESCE(status, ''), stop_requested_at IS NOT NULL, ended_at IS NOT NULL
		  FROM conversations WHERE id = ?`, convID).Scan(&st.conversationStatus, &st.stopRequested, &st.ended); err != nil {
		t.Fatalf("read conversation %s: %v", convID, err)
	}
	return st
}

// seedRunOnTask stages one blueprint_run in blueprintStatus with its step-0
// conversation on the task, in the given STORED conversation status ("" =
// SQL NULL, the mid-flight state; dbtest.SeedConcluded, a recorded verdict).
// This is the shape a fired trigger leaves behind; the router's own delegation
// path is not the thing under test here.
func seedRunOnTask(t *testing.T, database *sql.DB, taskID, blueprintStatus, convStatus string) (blueprintRunID, convID string) {
	t.Helper()
	promptID := uuid.New().String()
	createTestPrompt(t, database, domain.Prompt{ID: promptID, Name: "Close", Body: "x", Source: "user"})
	blueprintID := insertBlueprintForTeam(t, database, uuid.New().String(), promptID, runmode.LocalDefaultTeamID)

	stepPlan, err := domain.MarshalStepPlan([]domain.BlueprintPlanStep{
		{StepIndex: 0, PromptID: promptID, PromptName: "Close", PromptBody: "x", Source: "user"},
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
	stored, concluded := dbtest.SeedStatus(convStatus)
	if stored != "" {
		status = stored
	}
	if _, err := database.Exec(`
		INSERT INTO conversations (id, task_id, prompt_id, status, model, trigger_type,
		                           team_id, visibility, creator_user_id, origin,
		                           blueprint_run_id, blueprint_step_index, completed_at)
		VALUES (?, ?, ?, ?, 'm', 'manual', ?, 'team', ?, 'blueprint', ?, 0,
		        CASE WHEN ? THEN CURRENT_TIMESTAMP END)
	`, convID, taskID, promptID, status, runmode.LocalDefaultTeamID,
		runmode.LocalDefaultUserID, blueprintRunID, concluded); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return blueprintRunID, convID
}

// failRun leaves the run as a step that failed leaves it once its blueprint
// has acted on the failure: the conversation and the run both failed.
func failRun(t *testing.T, database *sql.DB, runID, convID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE conversations SET status = ? WHERE id = ?`, domain.StatusFailed, convID); err != nil {
		t.Fatalf("fail conversation %s: %v", convID, err)
	}
	if _, err := database.Exec(`UPDATE blueprint_runs SET status = 'failed' WHERE id = ?`, runID); err != nil {
		t.Fatalf("fail blueprint run %s: %v", runID, err)
	}
}

// claimNext asks the real dispatcher scan whether anything is drivable: the
// question that matters for work on a closed task, since the claim gate is
// what drives a conversation on.
func claimNext(t *testing.T, database *sql.DB) *domain.Conversation {
	t.Helper()
	conv, err := sqlitestore.New(database).ConversationQueue.ClaimNextConversation(context.Background(), "test-executor", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
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
		"github", "https://github.com", sourceID, "", "pr", "PR", "https://example.com")
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

// TestPlainClose_TerminatingCloseLeavesTheWorkAsItWas is the headline. A
// merge closes the task at once, under a conversation that is working or
// parked on a person, and writes nothing on either: the working one is told
// and is still driven, and the parked one is still resumable.
func TestPlainClose_TerminatingCloseLeavesTheWorkAsItWas(t *testing.T) {
	for _, tc := range []struct {
		name, convStatus string
		working          bool
	}{
		{name: "working", convStatus: "", working: true},
		{name: "parked", convStatus: domain.StatusOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			sp := &stubDelegator{}
			r.spawner = sp
			entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#merged-"+tc.name)
			brID, convID := seedRunOnTask(t, database, taskID, "running", tc.convStatus)
			before := readWorkState(t, database, brID, convID)

			enqueueMerged(t, database, entityID)
			if err := r.drainEventQueue(context.Background()); err != nil {
				t.Fatalf("drainEventQueue: %v", err)
			}

			if status, reason, _ := taskRow(t, database, taskID); status != "done" || reason != "entity_closed" {
				t.Fatalf("task = (%q, %q), want closed with entity_closed at once", status, reason)
			}
			if ev := heldClose(t, database, taskID); ev != "" {
				t.Errorf("held close = %q, want none: work on a task holds no close", ev)
			}
			if n := closeAuditCount(t, database, taskID); n != 1 {
				t.Errorf("close-audit rows = %d, want 1", n)
			}
			if after := readWorkState(t, database, brID, convID); after != before {
				t.Errorf("the close changed the work on the task:\n before %+v\n after  %+v", before, after)
			}
			var wantNoted []string
			if tc.working {
				wantNoted = []string{convID}
			}
			if got := sp.notedIDs(); !slices.Equal(got, wantNoted) {
				t.Errorf("told = %v, want %v", got, wantNoted)
			}

			if !tc.working {
				// A person picks the parked step back up under the closed task.
				flipped, err := sqlitestore.New(database).Conversations.MarkQueuedForResume(t.Context(), runmode.LocalDefaultOrgID, convID)
				if err != nil || !flipped {
					t.Fatalf("MarkQueuedForResume = (%v, %v), want (true, nil)", flipped, err)
				}
			}
			if conv := claimNext(t, database); conv == nil || conv.ID != convID {
				t.Errorf("ClaimNextConversation = %v, want %s: a close must leave the task's work drivable", conv, convID)
			}
		})
	}
}

// TestPlainClose_SiblingCloseLeavesTheWorkAsItWas: CI passing closes the CI
// failure's task at once, under the conversation working on it, which it
// tells and leaves working.
func TestPlainClose_SiblingCloseLeavesTheWorkAsItWas(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#passed-working")
	brID, convID := seedRunOnTask(t, database, taskID, "running", "")
	before := readWorkState(t, database, brID, convID)
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
	)

	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}

	status, reason, eventType := taskRow(t, database, taskID)
	if status != "done" || reason != db.SiblingEventCloseReason || eventType != domain.EventGitHubPRCICheckPassed {
		t.Fatalf("task = (%q, %q, %q), want closed by the pass at once", status, reason, eventType)
	}
	if ev := heldClose(t, database, taskID); ev != "" {
		t.Errorf("held close = %q, want none", ev)
	}
	if after := readWorkState(t, database, brID, convID); after != before {
		t.Errorf("the close changed the work on the task:\n before %+v\n after  %+v", before, after)
	}
	if got := sp.notedIDs(); !slices.Equal(got, []string{convID}) {
		t.Errorf("told = %v, want [%s]", got, convID)
	}
	if conv := claimNext(t, database); conv == nil || conv.ID != convID {
		t.Errorf("ClaimNextConversation = %v, want %s: the conversation keeps working", conv, convID)
	}
}

// TestPlainClose_FinishedBlueprintStaysResumable: a blueprint that finished
// before its task closed is left as it was, so a follow-up may resume its
// final step afterwards.
func TestPlainClose_FinishedBlueprintStaysResumable(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#finished-blueprint")
	brID, convID := seedRunOnTask(t, database, taskID, "completed", dbtest.SeedConcluded)
	before := readWorkState(t, database, brID, convID)

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Fatalf("active tasks = %d, want 0", n)
	}
	if got := sp.notedIDs(); len(got) != 0 {
		t.Errorf("told = %v, want nobody: nothing was working", got)
	}
	if after := readWorkState(t, database, brID, convID); after != before {
		t.Fatalf("the close changed the finished run:\n before %+v\n after  %+v", before, after)
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

// TestPlainClose_ReplayedCloseWritesNothingAgain pins the replay. The close
// phase is a routing obligation, so a failed pass replays the whole event; the
// tasks it already closed are terminal by then. That second pass leaves them
// as they are: no second audit row, and no note to a run that started under
// the closed task in between.
func TestPlainClose_ReplayedCloseWritesNothingAgain(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{}
	r.spawner = sp

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "https://github.com", "owner/repo#replayed-close", "", "pr", "PR", "https://example.com")
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
	seedPRCheckRuns(t, database, entity.ID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
		domain.CheckRun{ID: 2, Name: "lint", Status: "completed", Conclusion: "success"},
	)

	// One task's close fails, so the event requeues — the close phase's own
	// obligation retry, which is the real path a replay takes rather than a
	// re-enqueue staged by hand.
	r.tasks = &taskCloseOutageStore{TaskStore: testTaskStore(database), failTaskID: doomed, o: &outage{remaining: 1}}
	enqueueCIPassed(t, database, entity.ID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if status, _, _ := taskRow(t, database, closedFirst); status != "done" {
		t.Fatalf("setup: the first pass left %s %q, want done", closedFirst, status)
	}
	if status, _, _ := taskRow(t, database, doomed); status == "done" {
		t.Fatalf("setup: the first pass closed %s despite the outage", doomed)
	}

	// Between the passes, a run goes active under the task that already
	// closed. The replay must leave it entirely alone.
	brID, convID := seedRunOnTask(t, database, closedFirst, "running", "")
	before := readWorkState(t, database, brID, convID)

	ripenQueue(t, database)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue on the replay: %v", err)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 0 {
		t.Fatalf("active tasks after the replay = %d, want 0", n)
	}
	if after := readWorkState(t, database, brID, convID); after != before {
		t.Errorf("the replayed close changed a run started after it:\n before %+v\n after  %+v", before, after)
	}
	if got := sp.notedIDs(); len(got) != 0 {
		t.Errorf("told = %v, want nobody: the replay closed only a task nothing works on", got)
	}
	if n := closeAuditCount(t, database, closedFirst); n != 1 {
		t.Errorf("close-audit rows on the already-closed task = %d, want 1", n)
	}
}
