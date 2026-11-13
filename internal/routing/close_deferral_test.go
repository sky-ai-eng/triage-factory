package routing

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/google/uuid"
	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// One thing holds an event's close: a deliverable on the task awaiting a
// person, on a pull request still open. The sweep applies the close once the
// person resolves it, if it still holds then. A conversation working on the
// task holds nothing.

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

// resolveDrafts marks the conversation's draft pull requests open, as a
// person marking one ready does.
func resolveDrafts(t *testing.T, database *sql.DB, convID string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE deliverables SET state = 'open' WHERE conversation_id = ? AND state = 'draft'`, convID); err != nil {
		t.Fatalf("resolve the drafts on %s: %v", convID, err)
	}
}

// heldCIFixture is a ci_check_failed task whose finished run left a draft
// pull request for a person, with a second run working on it, whose close a
// CI pass has held; and a sibling ci_check_failed task with nothing on it
// that the same pass closed.
type heldCIFixture struct {
	r                *Router
	database         *sql.DB
	sp               *stubDelegator
	entityID, taskID string
	otherID          string
	// draftConvID is the finished run's conversation, which holds the draft.
	draftConvID string
	// runID and convID are the run still working on the task.
	runID, convID string
	eventID       string
}

func newHeldCIFixture(t *testing.T, sourceID string) heldCIFixture {
	t.Helper()
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, sourceID)
	otherID := secondCIFailedTask(t, r, database, entityID, taskID)
	_, draftConvID := seedRunOnTask(t, database, taskID, "completed", dbtest.SeedConcluded)
	seedDraftPullRequest(t, database, draftConvID, entityID)
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
	return heldCIFixture{r: r, database: database, sp: sp, entityID: entityID, taskID: taskID, otherID: otherID,
		draftConvID: draftConvID, runID: runID, convID: convID, eventID: eventID}
}

// TestCloseHold_DeliverableAwaitingAPersonHoldsTheClose is the headline: the
// pass holds the close of the task whose draft awaits a person, writes
// nothing on the work under it, and tells the working conversation that the
// event resolved the situation, not that the task closed: it is still open.
// The sibling task with no deliverable closes now.
func TestCloseHold_DeliverableAwaitingAPersonHoldsTheClose(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#held")

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Fatal("the task closed while its draft awaits a person")
	}
	if got := readWorkState(t, f.database, f.runID, f.convID); got.cancelRequested || got.stopRequested || got.runStatus != "running" {
		t.Errorf("the hold changed the work on the task: %+v", got)
	}
	if n := closeAuditCount(t, f.database, f.taskID); n != 0 {
		t.Errorf("close audit rows on the held task = %d, want 0", n)
	}
	if got := f.sp.notedIDs(); len(got) != 0 {
		t.Errorf("told of a close = %v, want nobody: a held close closed nothing", got)
	}
	if got := f.sp.heldNotedIDs(); !slices.Equal(got, []string{f.convID}) {
		t.Errorf("told the close is held = %v, want [%s]", got, f.convID)
	}
	if status, reason, _ := taskRow(t, f.database, f.otherID); status != "done" || reason != "auto_closed_by_event" {
		t.Errorf("other task = (%q, %q), want closed by the event", status, reason)
	}
}

// TestCloseHold_SecondClosingEventTakesTheHoldsPlace: a later closing event
// while the close is held takes its place, and the conversations, told when
// the hold began, are not told again.
func TestCloseHold_SecondClosingEventTakesTheHoldsPlace(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#held-twice")

	enqueueCIPassed(t, f.database, f.entityID, "lint")
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second pass: %v", err)
	}

	if ev := heldClose(t, f.database, f.taskID); ev == "" || ev == f.eventID {
		t.Errorf("held close = %q, want the second pass's event", ev)
	}
	if got := f.sp.heldNotedIDs(); len(got) != 1 {
		t.Errorf("told the close is held = %v, want the one note from when the hold began", got)
	}
	if got := f.sp.notedIDs(); len(got) != 0 {
		t.Errorf("told of a close = %v, want nobody while the close is held", got)
	}
}

// TestCloseHold_TerminatingCloseHoldsForADraftOnAnotherPullRequest: a merge
// closes the entity and the task with nothing awaiting a person, and holds
// the close of the task whose draft is on another pull request, still open.
// A draft on the merged pull request itself holds nothing.
func TestCloseHold_TerminatingCloseHoldsForADraftOnAnotherPullRequest(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{}
	r.spawner = sp
	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#merged-held")
	otherID := secondCIFailedTask(t, r, database, entityID, taskID)
	elsewhere, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "https://github.com", "owner/repo#still-open", "", "pr", "PR", "https://example.com/2")
	if err != nil {
		t.Fatalf("create the other pull request: %v", err)
	}
	_, convID := seedRunOnTask(t, database, taskID, "completed", dbtest.SeedConcluded)
	seedDraftPullRequest(t, database, convID, elsewhere.ID)
	_, otherConv := seedRunOnTask(t, database, otherID, "completed", dbtest.SeedConcluded)
	seedDraftPullRequest(t, database, otherConv, entityID)

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
		t.Error("the close of the task whose draft is on an open pull request was not held")
	}
	if status, _, _ := taskRow(t, database, taskID); status == "done" {
		t.Error("the task closed while its draft awaits a person")
	}
	if status, reason, _ := taskRow(t, database, otherID); status != "done" || reason != "entity_closed" {
		t.Errorf("other task = (%q, %q), want closed with entity_closed", status, reason)
	}
}

// TestCloseHold_ReassignmentClosesAtOnce pins the relation's setting: the
// task the previous assignment made closes at once as reassigned, under a
// working conversation it leaves working and a draft that would hold any
// other close, so the same event mints the new assignee's team a task of its
// own instead of bumping the old one.
func TestCloseHold_ReassignmentClosesAtOnce(t *testing.T) {
	database, router, _, teamB, entityID := reassignFixture(t)
	sp := &stubDelegator{}
	router.spawner = sp
	mustHandle(t, router, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	first := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(first) != 1 {
		t.Fatalf("after the first assignment: %d tasks, want 1", len(first))
	}
	runID, convID := seedRunOnTask(t, database, first[0].ID, "running", "")
	seedDraftPullRequest(t, database, convID, entityID)
	before := readWorkState(t, database, runID, convID)

	mustHandle(t, router, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-bob"))

	status, reason, eventType := taskRow(t, database, first[0].ID)
	if status != "done" || reason != dbpkg.ReassignedCloseReason || eventType != domain.EventLinearIssueAssigned {
		t.Errorf("the old assignment's task = (%q, %q, %q), want done as reassigned by the assignment", status, reason, eventType)
	}
	if ev := heldClose(t, database, first[0].ID); ev != "" {
		t.Errorf("held close = %q, want none: a reassignment never holds", ev)
	}
	if after := readWorkState(t, database, runID, convID); after != before {
		t.Errorf("the reassignment changed the work on the old task:\n before %+v\n after  %+v", before, after)
	}
	if got := sp.notedIDs(); !slices.Equal(got, []string{convID}) {
		t.Errorf("noted = %v, want [%s]", got, convID)
	}
	active := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(active) != 1 || active[0].ID == first[0].ID || teamIDValue(&active[0]) != teamB {
		t.Errorf("active assigned tasks = %+v, want one new task owned by team B", active)
	}
}

// TestCloseHold_OwnEventArrivingAgainCancelsTheHold: the task's own event
// arriving again says the situation is live again, so once the person has
// resolved the draft the sweep leaves the task open.
func TestCloseHold_OwnEventArrivingAgainCancelsTheHold(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#held-then-failed")

	enqueueCIFailedCheck(t, f.database, f.entityID, "build")
	if err := f.r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the second failure: %v", err)
	}
	resolveDrafts(t, f.database, f.draftConvID)
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
	resolveDrafts(t, f.database, f.draftConvID)
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

// TestCloseHoldSweep_LeavesATaskHeldWhileItsDraftAwaitsAPerson: until the
// person resolves the draft, the sweep closes nothing and keeps the hold.
func TestCloseHoldSweep_LeavesATaskHeldWhileItsDraftAwaitsAPerson(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-awaiting")

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose draft awaits a person")
	}
	if ev := heldClose(t, f.database, f.taskID); ev != f.eventID {
		t.Errorf("held close = %q, want %s", ev, f.eventID)
	}
}

// TestCloseHoldSweep_AppliesTheCloseOnceThePersonResolvesTheDraft: the close
// applies through the plain close once nothing awaits a person: the event's
// reason and type, and its audit row. The conversation still working on the
// task is left working, and is told then.
func TestCloseHoldSweep_AppliesTheCloseOnceThePersonResolvesTheDraft(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-applies")
	before := readWorkState(t, f.database, f.runID, f.convID)
	resolveDrafts(t, f.database, f.draftConvID)

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
	if after := readWorkState(t, f.database, f.runID, f.convID); after != before {
		t.Errorf("the applied close changed the work on the task:\n before %+v\n after  %+v", before, after)
	}
	// The hold told the conversation the situation resolved; this is the
	// note that the task closed.
	if got := f.sp.notedIDs(); !slices.Equal(got, []string{f.convID}) {
		t.Errorf("told of the close = %v, want [%s] once it applied", got, f.convID)
	}
}

// TestCloseHoldSweep_DropsAHoldThatNoLongerHolds: CI failing again by the
// time the person resolves the draft means the pass no longer resolves the
// task, so the hold is cleared and nothing closes.
func TestCloseHoldSweep_DropsAHoldThatNoLongerHolds(t *testing.T) {
	f := newHeldCIFixture(t, "owner/repo#sweep-dropped")
	seedPRCheckRuns(t, f.database, f.entityID,
		domain.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"},
	)
	resolveDrafts(t, f.database, f.draftConvID)

	f.r.sweepCloseDeferrals(t.Context())

	if status, _, _ := taskRow(t, f.database, f.taskID); status == "done" {
		t.Error("the sweep closed a task whose close no longer holds")
	}
	if ev := heldClose(t, f.database, f.taskID); ev != "" {
		t.Errorf("held close = %q, want cleared", ev)
	}
}

// TestCloseHold_FiringsStartOnAHeldTask: a task whose close is held is an
// open task, and an automatic firing starts new work on it as on any other.
// The preflight every auto-fire runs proceeds, and the firing worker fires
// the task's queued firing.
func TestCloseHold_FiringsStartOnAHeldTask(t *testing.T) {
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
	if _, err := database.Exec(`UPDATE tasks SET close_deferred_event_id = ?, close_deferred_at = CURRENT_TIMESTAMP WHERE id = ?`, eventID, taskID); err != nil {
		t.Fatalf("hold the task's close: %v", err)
	}

	task, err := testTaskStore(database).GetSystem(ctx, org, taskID)
	if err != nil || task == nil {
		t.Fatalf("read task: task=%v err=%v", task, err)
	}
	if _, proceed, err := r.autoDelegatePreflight(ctx, org, task, *trigger, entityID, ""); err != nil || !proceed {
		t.Errorf("preflight on a held task = (proceed %v, err %v), want it to proceed", proceed, err)
	}
	drainOnce(t, r)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	if rows[0].SkipReason != "" {
		t.Errorf("the held task's firing was skipped as %q, want it fired", rows[0].SkipReason)
	}
	if stub.calls != 1 {
		t.Errorf("Delegate called %d times on a held task, want 1", stub.calls)
	}
}
