package routing

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/google/uuid"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// An event close that finds a working conversation on its task holds the
// close and tells the conversation instead of stopping it; the sweep applies
// the close once nothing on the task is working, if it still holds then.

// heldClose reads a task's held close event, "" for none.
func heldClose(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	var ev sql.NullString
	if err := database.QueryRow(`SELECT close_deferred_event_id FROM tasks WHERE id = ?`, taskID).Scan(&ev); err != nil {
		t.Fatalf("read task %s's held close: %v", taskID, err)
	}
	return ev.String
}

func taskRow(t *testing.T, database *sql.DB, taskID string) (status, closeReason, closeEventType string) {
	t.Helper()
	var reason, eventType sql.NullString
	if err := database.QueryRow(`SELECT status, close_reason, close_event_type FROM tasks WHERE id = ?`, taskID).Scan(&status, &reason, &eventType); err != nil {
		t.Fatalf("read task %s: %v", taskID, err)
	}
	return status, reason.String, eventType.String
}

// secondCIFailedTask mints a ci_check_failed task for another check on the
// entity: a task the same pass closes that nothing is working on.
func secondCIFailedTask(t *testing.T, r *Router, database *sql.DB, entityID, firstTaskID string) string {
	t.Helper()
	enqueueCIFailedCheck(t, database, entityID, "lint")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second ci event: %v", err)
	}
	for _, task := range activeCIFailedTasks(t, database, entityID) {
		if task.ID != firstTaskID {
			return task.ID
		}
	}
	t.Fatal("setup: no second ci_check_failed task")
	return ""
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

// seedDraftPullRequest gives the conversation a draft pull request
// deliverable about the pull request entityID names, one that awaits a
// person's verdict while that entity is open.
func seedDraftPullRequest(t *testing.T, database *sql.DB, convID, entityID string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT INTO deliverables (id, conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key, entity_id)
		SELECT ?, id, org_id, team_id, 'github', 'pull_request', 'github.com', 'acme/widget#7', 'draft', 'held-close-' || id, ?
		  FROM conversations WHERE id = ?
	`, uuid.New().String(), entityID, convID); err != nil {
		t.Fatalf("seed draft pull request on %s: %v", convID, err)
	}
}

// heldCIFixture is a ci_check_failed task with a working conversation whose
// close a CI pass has held, and a sibling ci_check_failed task with nothing
// on it that the same pass closed.
type heldCIFixture struct {
	r                *Router
	database         *sql.DB
	sp               *stoppingSpawner
	entityID, taskID string
	otherID          string
	runID, convID    string
	eventID          string
}

func newHeldCIFixture(t *testing.T, sourceID string) heldCIFixture {
	t.Helper()
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, sourceID)
	otherID := secondCIFailedTask(t, r, database, entityID, taskID)
	runID, convID := seedRunOnTask(t, database, taskID, "running", "")
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "success"},
		domain.CheckRun{ID: 2, Name: "lint", Status: "completed", Conclusion: "success"},
	)
	enqueueCIPassed(t, database, entityID, "build")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}
	eventID := heldClose(t, database, taskID)
	if eventID == "" {
		t.Fatal("setup: the pass held no close")
	}
	return heldCIFixture{r: r, database: database, sp: sp, entityID: entityID, taskID: taskID, otherID: otherID, runID: runID, convID: convID, eventID: eventID}
}

// TestCloseHold_WorkingConversationHoldsTheClose is the headline: the pass
// holds the close of the task a conversation is working on, stops nothing,
// tears nothing down, and tells the conversation once. The sibling task
// nothing is working on closes now.
func TestCloseHold_WorkingConversationHoldsTheClose(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#held")

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Fatal("the worked-on task closed under its conversation")
	}
	if got := f.sp.stoppedIDs(); len(got) != 0 {
		t.Errorf("stopped = %v, want nothing", got)
	}
	if cancelRequested(t, f.database, f.runID) {
		t.Error("the run was called off")
	}
	if n := closeAuditCount(t, f.database, f.taskID); n != 0 {
		t.Errorf("close audit rows on the held task = %d, want 0", n)
	}
	if got := f.sp.notedIDs(); !slices.Equal(got, []string{f.convID}) {
		t.Errorf("noted = %v, want [%s]", got, f.convID)
	}
	if status, reason, _ := taskRow(t, f.database, f.otherID); status != "done" || reason != "auto_closed_by_event" {
		t.Errorf("other task = (%q, %q), want closed by the event", status, reason)
	}
	if got := f.sp.tornDownCopy(); !slices.Equal(got, []string{f.otherID}) {
		t.Errorf("teardowns = %v, want only the closed task %s", got, f.otherID)
	}
}

// TestCloseHold_SecondClosingEventSendsNoSecondNote: a later closing event
// while the close is held takes its place, and the conversations, told
// already, are not told again.
func TestCloseHold_SecondClosingEventSendsNoSecondNote(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#held-twice")

	enqueueCIPassed(t, f.database, f.entityID, "lint")
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second pass: %v", err)
	}

	if ev := heldClose(t, f.database, f.taskID); ev == "" || ev == f.eventID {
		t.Errorf("held close = %q, want the second pass's event", ev)
	}
	if got := f.sp.notedIDs(); len(got) != 1 {
		t.Errorf("noted = %v, want the one note the first pass sent", got)
	}
}

// TestCloseHold_TerminatingCloseHoldsAndTells: a merge closes the entity and
// its idle task, and holds the worked-on task's close and tells its
// conversation, in the same pass.
func TestCloseHold_TerminatingCloseHoldsAndTells(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stoppingSpawner{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#merged-held")
	otherID := secondCIFailedTask(t, r, database, entityID, taskID)
	_, convID := seedRunOnTask(t, database, taskID, "running", "")

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the merge: %v", err)
	}

	var state string
	if err := database.QueryRow(`SELECT state FROM entities WHERE id = ?`, entityID).Scan(&state); err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if state != "closed" {
		t.Errorf("entity state = %q, want closed", state)
	}
	if heldClose(t, database, taskID) == "" {
		t.Error("the worked-on task's close was not held")
	}
	if got := sp.notedIDs(); !slices.Equal(got, []string{convID}) {
		t.Errorf("noted = %v, want [%s]", got, convID)
	}
	if status, reason, _ := taskRow(t, database, otherID); status != "done" || reason != "entity_closed" {
		t.Errorf("other task = (%q, %q), want closed with entity_closed", status, reason)
	}
}

// TestCloseHold_ReassignmentClosesAtOnce pins the exception: a task of the
// closing event's own type closes under a working conversation, which is
// stopped, so the new assignment mints the new assignee's team a task of its
// own instead of bumping the old one.
func TestCloseHold_ReassignmentClosesAtOnce(t *testing.T) {
	database, router, _, teamB, entityID := reassignFixture(t)
	sp := &stoppingSpawner{}
	router.spawner = sp
	mustHandle(t, router, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	first := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(first) != 1 {
		t.Fatalf("after the first assignment: %d tasks, want 1", len(first))
	}
	_, convID := seedRunOnTask(t, database, first[0].ID, "running", "")

	mustHandle(t, router, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-bob"))

	if status, _, _ := taskRow(t, database, first[0].ID); status != "done" {
		t.Errorf("the old assignment's task = %q, want done", status)
	}
	if got := sp.stoppedIDs(); !slices.Equal(got, []string{convID}) {
		t.Errorf("stopped = %v, want [%s]", got, convID)
	}
	active := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(active) != 1 || active[0].ID == first[0].ID || teamIDValue(&active[0]) != teamB {
		t.Errorf("active assigned tasks = %+v, want one new task owned by team B", active)
	}
}

// TestCloseHold_OwnEventArrivingAgainCancelsTheHold: the task's own event
// arriving again says the situation is live again, so once nothing is
// working the sweep leaves the task open.
func TestCloseHold_OwnEventArrivingAgainCancelsTheHold(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#held-then-failed")

	enqueueCIFailedCheck(t, f.database, f.entityID, "build")
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second failure: %v", err)
	}
	failRun(t, f.database, f.runID, f.convID)
	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task its own event reopened")
	}
	if ev := heldClose(t, f.database, f.taskID); ev != "" {
		t.Errorf("held close = %q after the task's own event, want none", ev)
	}
}

// TestCloseHoldSweep_LeavesAHoldCancelledSinceItsReadAlone: the sweep settles
// the holds it listed, and the task's own event can cancel one in between.
// The pass still admitting the close by then (the checks still pass) does not
// close the task or bring the hold back.
func TestCloseHoldSweep_LeavesAHoldCancelledSinceItsReadAlone(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-cancelled")
	read := domain.TaskCloseDeferral{OrgID: runmode.LocalDefaultOrgID, TaskID: f.taskID, EventID: f.eventID}

	enqueueCIFailedCheck(t, f.database, f.entityID, "build")
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second failure: %v", err)
	}
	failRun(t, f.database, f.runID, f.convID)
	closed, err := f.r.settleCloseDeferral(t.Context(), read)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}

	if status, _, _ := taskRow(t, f.database, f.taskID); closed || status == "done" {
		t.Error("the sweep closed a task on a hold its own event cancelled")
	}
	if ev := heldClose(t, f.database, f.taskID); ev != "" {
		t.Errorf("held close = %q after settling a cancelled hold, want none", ev)
	}
}

// TestCloseHoldSweep_LeavesAWorkingTaskHeld: while the conversation works,
// the sweep closes nothing and stops nothing.
func TestCloseHoldSweep_LeavesAWorkingTaskHeld(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-working")

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task a conversation is working on")
	}
	if ev := heldClose(t, f.database, f.taskID); ev != f.eventID {
		t.Errorf("held close = %q, want %s", ev, f.eventID)
	}
	if got := f.sp.stoppedIDs(); len(got) != 0 {
		t.Errorf("stopped = %v, want nothing", got)
	}
}

// TestCloseHoldSweep_AppliesTheCloseOnceNothingWorks: the close applies
// through the ordinary path once the conversation stops working: the event's
// reason and type, its audit row, and the deliverable teardown.
func TestCloseHoldSweep_AppliesTheCloseOnceNothingWorks(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-applies")
	failRun(t, f.database, f.runID, f.convID)

	f.r.sweepCloseDeferrals(t.Context())

	status, reason, eventType := taskRow(t, f.database, f.taskID)
	if status != "done" || reason != "auto_closed_by_event" || eventType != domain.EventGitHubPRCICheckPassed {
		t.Errorf("task = (%q, %q, %q), want done by the pass", status, reason, eventType)
	}
	var audit int
	if err := f.database.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND event_id = ? AND kind = 'closed'`, f.taskID, f.eventID).Scan(&audit); err != nil {
		t.Fatalf("read the close audit row: %v", err)
	}
	if audit != 1 {
		t.Errorf("close audit rows naming the pass = %d, want 1", audit)
	}
	if !slices.Contains(f.sp.tornDownCopy(), f.taskID) {
		t.Errorf("teardowns = %v, want the held task %s among them", f.sp.tornDownCopy(), f.taskID)
	}
}

// TestCloseHoldSweep_WaitsForADeliverableAwaitingAPerson: once nothing works,
// a draft pull request the run left for a person keeps the close held, with
// nothing torn down, until the person resolves it; the next pass then closes
// the task.
func TestCloseHoldSweep_WaitsForADeliverableAwaitingAPerson(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-awaiting")
	failRun(t, f.database, f.runID, f.convID)
	seedDraftPullRequest(t, f.database, f.convID, f.entityID)

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose draft awaits a person")
	}
	if heldClose(t, f.database, f.taskID) != f.eventID {
		t.Error("the sweep dropped the close a draft awaiting a person holds")
	}
	if slices.Contains(f.sp.tornDownCopy(), f.taskID) {
		t.Error("the sweep tore down the draft awaiting a person")
	}

	if _, err := f.database.Exec(`UPDATE deliverables SET state = 'open' WHERE conversation_id = ?`, f.convID); err != nil {
		t.Fatalf("resolve the draft: %v", err)
	}
	f.r.sweepCloseDeferrals(t.Context())

	if status, reason, _ := taskRow(t, f.database, f.taskID); status != "done" || reason != "auto_closed_by_event" {
		t.Errorf("task = (%q, %q), want closed by the pass once the draft is resolved", status, reason)
	}
}

// TestCloseHoldSweep_DropsAHoldThatNoLongerHolds: CI failing again by the
// time nothing works means the pass no longer resolves the task, so the hold
// is cleared and nothing closes.
func TestCloseHoldSweep_DropsAHoldThatNoLongerHolds(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-dropped")
	seedPRCheckRuns(t, f.database, f.entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
	)
	failRun(t, f.database, f.runID, f.convID)

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose close no longer holds")
	}
	if ev := heldClose(t, f.database, f.taskID); ev != "" {
		t.Errorf("held close = %q, want cleared", ev)
	}
}

// TestCloseHold_FiringsSkipAHeldTask: no automatic firing starts new work on
// a task whose close is held. The preflight every auto-fire runs proceeds on
// the task until its close is held and declines it after, and the firing
// worker skips its queued firing as task_closed.
func TestCloseHold_FiringsSkipAHeldTask(t *testing.T) {
	database := newTestDB(t)
	ctx := t.Context()
	org := runmode.LocalDefaultOrgID
	stores := sqlitestore.New(database)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	agent, err := stores.Agents.GetForOrgSystem(ctx, org)
	if err != nil || agent == nil {
		t.Fatalf("read the org agent: agent=%v err=%v", agent, err)
	}
	if err := stores.TeamAgents.AddForTeam(ctx, org, runmode.LocalDefaultTeamID, agent.ID); err != nil {
		t.Fatalf("enable the bot for the team: %v", err)
	}
	stub := &stubDelegator{db: database}
	r := drainRouter(database, stores.PendingFirings, stub)
	trigger, err := stores.EventHandlers.GetSystem(ctx, org, triggerID)
	if err != nil || trigger == nil {
		t.Fatalf("read trigger: trigger=%v err=%v", trigger, err)
	}
	preflight := func() bool {
		t.Helper()
		task, err := testTaskStore(database).GetSystem(ctx, org, taskID)
		if err != nil || task == nil {
			t.Fatalf("read task: task=%v err=%v", task, err)
		}
		_, proceed, err := r.autoDelegatePreflight(ctx, org, task, *trigger, entityID, "")
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		return proceed
	}
	if !preflight() {
		t.Fatal("setup: the preflight declines the task before its close is held")
	}

	if _, err := database.Exec(`UPDATE tasks SET close_deferred_event_id = ? WHERE id = ?`, eventID, taskID); err != nil {
		t.Fatalf("hold the task's close: %v", err)
	}
	if preflight() {
		t.Error("the preflight proceeds on a held task")
	}
	drainOnce(t, r)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipTaskClosed)
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times on a held task", stub.calls)
	}
}
