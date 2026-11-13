package routing

import (
	"context"
	"database/sql"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A task a sibling event closed reopens when its own event recurs while one
// of its conversations is still working under a running blueprint, and the
// event is folded into that conversation. With nothing working, with a
// person's follow-up working on a finished blueprint, or after any other
// close, the event gets a new task.

// foldingDelegator is stubDelegator with a fold that lands: the event a busy
// task gate hands its live conversation is delivered, and the conversation
// recorded.
type foldingDelegator struct {
	*stubDelegator
	folded []string
}

func (d *foldingDelegator) StageOrDeliverAdditiveEvent(_ context.Context, _, conversationID, _, _ string, _ domain.NoteProvenance, _ delegate.AdditiveFiringRef) delegate.AdditiveDelivery {
	d.folded = append(d.folded, conversationID)
	return delegate.AdditiveDelivery{Outcome: delegate.InjectDeliveredLocal}
}

// reopenFixture is a ci_check_failed task whose trigger fired: the agent
// holds the claim, and its run's first conversation is working.
type reopenFixture struct {
	database      *sql.DB
	router        *Router
	pub           *fakeDispositionPublisher
	fold          *foldingDelegator
	entityID      string
	taskID        string
	runID, convID string
}

func newReopenFixture(t *testing.T, sourceID string) reopenFixture {
	t.Helper()
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedImmediateTrigger(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, "ci-reopen")
	enableTeamAutoDelegate(t, database, runmode.LocalDefaultTeamID)

	fold := &foldingDelegator{stubDelegator: &stubDelegator{db: database}}
	pub := &fakeDispositionPublisher{}
	deps := testRouterDeps(database)
	deps.Spawner = fold
	deps.Publisher = pub
	f := reopenFixture{database: database, router: newTestRouter(deps), pub: pub, fold: fold}

	f.entityID = reviewEntity(t, database, sourceID)
	emitCI(t, f.router, f.entityID, "aidan")
	active := activeCIFailedTasks(t, database, f.entityID)
	if len(active) != 1 || active[0].Status != "in_progress" || active[0].ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Fatalf("setup: active ci tasks = %+v, want one in progress under the agent's claim", active)
	}
	f.taskID = active[0].ID
	if err := database.QueryRow(`SELECT id, blueprint_run_id FROM conversations WHERE task_id = ?`, f.taskID).Scan(&f.convID, &f.runID); err != nil {
		t.Fatalf("setup: read the fired run's conversation: %v", err)
	}
	return f
}

// closeBySibling closes the fixture's task as CI passing closes it: the row
// the router's close writes, with the task's conversation left working.
func (f reopenFixture) closeBySibling(t *testing.T) {
	t.Helper()
	if _, err := testTaskStore(f.database).CloseSystem(t.Context(), runmode.LocalDefaultOrgID, f.taskID, dbpkg.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed); err != nil {
		t.Fatalf("close the task as a CI pass does: %v", err)
	}
}

// lastDisposition is the routing disposition of the most recent event.
func (f reopenFixture) lastDisposition(t *testing.T) events.SystemRoutingDispositionMetadata {
	t.Helper()
	got := f.pub.eventsCopy()
	if len(got) == 0 {
		t.Fatal("no routing disposition published")
	}
	return decodeDisposition(t, got[len(got)-1].MetadataJSON)
}

func (f reopenFixture) ciTaskIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.database.Query(`SELECT id FROM tasks WHERE entity_id = ? AND event_type = ? ORDER BY created_at, id`, f.entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil {
		t.Fatalf("list ci tasks: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan ci task: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list ci tasks: %v", err)
	}
	return ids
}

// TestReopen_OwnEventReopensTheTaskClosedUnderWorkingWork is the headline: CI
// passes and closes the task, then fails again on the agent's next push while
// its conversation is still working. The same task reopens under the claim it
// had, the failure reaches the working conversation, and nothing new starts.
func TestReopen_OwnEventReopensTheTaskClosedUnderWorkingWork(t *testing.T) {
	f := newReopenFixture(t, "owner/repo#reopened")
	f.closeBySibling(t)
	if status, reason, _ := taskRow(t, f.database, f.taskID); status != "done" || reason != dbpkg.SiblingEventCloseReason {
		t.Fatalf("setup: task = (%q, %q), want closed by the pass", status, reason)
	}

	emitCI(t, f.router, f.entityID, "aidan")

	disp := f.lastDisposition(t)
	if disp.Disposition != events.DispositionTaskReopened || disp.TaskID != f.taskID {
		t.Errorf("disposition = %q on task %q, want %q on %q", disp.Disposition, disp.TaskID, events.DispositionTaskReopened, f.taskID)
	}
	if disp.TriggersFired != 0 || disp.InjectedConversationID != f.convID {
		t.Errorf("triggers fired = %d, injected conversation = %q, want 0 and the working conversation %q", disp.TriggersFired, disp.InjectedConversationID, f.convID)
	}
	if got := f.ciTaskIDs(t); !slices.Equal(got, []string{f.taskID}) {
		t.Errorf("ci tasks on the entity = %v, want only the reopened %s", got, f.taskID)
	}
	task, err := testTaskStore(f.database).Get(t.Context(), runmode.LocalDefaultOrgID, f.taskID)
	if err != nil || task == nil {
		t.Fatalf("re-read task: %v", err)
	}
	if task.Status != "in_progress" || task.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("reopened task = status %q claimed by %q, want in_progress under the agent's claim", task.Status, task.ClaimedByAgentID)
	}
	if task.ClosedAt != nil || task.CloseReason != "" || task.CloseEventType != "" {
		t.Errorf("close columns after the reopen = (%v, %q, %q), want cleared", task.ClosedAt, task.CloseReason, task.CloseEventType)
	}
	if !slices.Equal(f.fold.folded, []string{f.convID}) {
		t.Errorf("folded into = %v, want the working conversation %s", f.fold.folded, f.convID)
	}
	var kind string
	if err := f.database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, f.taskID, disp.EventID).Scan(&kind); err != nil {
		t.Fatalf("read the recurrence's task_event: %v", err)
	}
	if kind != "injected" {
		t.Errorf("task_events.kind for the recurrence = %q, want injected", kind)
	}
	if n := atomic.LoadInt64(&f.fold.calls); n != 1 {
		t.Errorf("delegations = %d, want only the first event's", n)
	}
	var runs int
	if err := f.database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id IN (SELECT id FROM tasks WHERE entity_id = ?)`, f.entityID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("blueprint runs on the entity = %d, want the one already working", runs)
	}
}

// TestReopen_BetweenStepsQueuesTheEventForTheNextStep: the recurrence lands
// after one blueprint step concluded and before the next starts. The run is
// still working, so the task reopens, but no conversation is live to fold the
// event into. It is queued on the reopened task for the next step, and no
// second run starts beside the one already working.
func TestReopen_BetweenStepsQueuesTheEventForTheNextStep(t *testing.T) {
	f := newReopenFixture(t, "owner/repo#reopened-between-steps")
	if _, err := f.database.Exec(`UPDATE conversations SET status = 'open', completed_at = CURRENT_TIMESTAMP WHERE id = ?`, f.convID); err != nil {
		t.Fatalf("conclude the step: %v", err)
	}
	f.closeBySibling(t)

	emitCI(t, f.router, f.entityID, "aidan")

	disp := f.lastDisposition(t)
	if disp.Disposition != events.DispositionTaskReopened || disp.TaskID != f.taskID {
		t.Errorf("disposition = %q on task %q, want %q on %q", disp.Disposition, disp.TaskID, events.DispositionTaskReopened, f.taskID)
	}
	if got := f.ciTaskIDs(t); !slices.Equal(got, []string{f.taskID}) {
		t.Errorf("ci tasks on the entity = %v, want only the reopened %s", got, f.taskID)
	}
	if len(f.fold.folded) != 0 {
		t.Errorf("folded into = %v, want nothing: no conversation is live between steps", f.fold.folded)
	}
	var runs int
	if err := f.database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, f.taskID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("blueprint runs on the task = %d, want the one already working", runs)
	}
	firings, err := sqlitestore.New(f.database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, f.entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(firings) != 1 || firings[0].TaskID != f.taskID || firings[0].TriggeringEventID != disp.EventID {
		t.Errorf("pending firings = %+v, want the recurrence queued on the reopened task", firings)
	}
}

// TestReopen_NothingWorkingCreatesANewTask: the same recurrence after the
// task's run ended leaves the closed task closed and creates a new one, which
// starts its own run.
func TestReopen_NothingWorkingCreatesANewTask(t *testing.T) {
	f := newReopenFixture(t, "owner/repo#not-reopened")
	failRun(t, f.database, f.runID, f.convID)
	f.closeBySibling(t)

	emitCI(t, f.router, f.entityID, "aidan")

	disp := f.lastDisposition(t)
	if disp.Disposition != events.DispositionTaskCreated || disp.TaskID == f.taskID {
		t.Errorf("disposition = %q on task %q, want a new task created", disp.Disposition, disp.TaskID)
	}
	if got := f.ciTaskIDs(t); len(got) != 2 {
		t.Errorf("ci tasks on the entity = %v, want the closed one and a new one", got)
	}
	if status, reason, _ := taskRow(t, f.database, f.taskID); status != "done" || reason != dbpkg.SiblingEventCloseReason {
		t.Errorf("the closed task = (%q, %q), want it left closed by the pass", status, reason)
	}
	if len(f.fold.folded) != 0 {
		t.Errorf("folded into = %v, want nothing: no conversation is working", f.fold.folded)
	}
	if n := atomic.LoadInt64(&f.fold.calls); n != 2 {
		t.Errorf("delegations = %d, want the new task's run beside the first", n)
	}
}

// TestReopen_FollowUpOnAFinishedBlueprintCreatesANewTask: the recurrence lands
// while a person's follow-up on the task's finished blueprint is queued. The
// conversation is working, but no run finish is ahead of it to close a
// reopened task again, so the closed task stays closed, the event is not
// folded into the person's thread, and a new task takes it.
func TestReopen_FollowUpOnAFinishedBlueprintCreatesANewTask(t *testing.T) {
	f := newReopenFixture(t, "owner/repo#follow-up-not-reopened")
	if _, err := f.database.Exec(`UPDATE blueprint_runs SET status = ? WHERE id = ?`, domain.BlueprintRunStatusCompleted, f.runID); err != nil {
		t.Fatalf("finish the blueprint: %v", err)
	}
	f.closeBySibling(t)

	emitCI(t, f.router, f.entityID, "aidan")

	disp := f.lastDisposition(t)
	if disp.Disposition != events.DispositionTaskCreated || disp.TaskID == f.taskID {
		t.Errorf("disposition = %q on task %q, want a new task created", disp.Disposition, disp.TaskID)
	}
	if got := f.ciTaskIDs(t); len(got) != 2 {
		t.Errorf("ci tasks on the entity = %v, want the closed one and a new one", got)
	}
	if status, reason, _ := taskRow(t, f.database, f.taskID); status != "done" || reason != dbpkg.SiblingEventCloseReason {
		t.Errorf("the closed task = (%q, %q), want it left closed by the pass", status, reason)
	}
	if len(f.fold.folded) != 0 {
		t.Errorf("folded into = %v, want nothing: the follow-up is the person's thread", f.fold.folded)
	}
	if n := atomic.LoadInt64(&f.fold.calls); n != 2 {
		t.Errorf("delegations = %d, want the new task's run beside the first", n)
	}
}

// TestReopen_ReassignmentLeavesTheOldTaskClosed: a reassignment closes the
// previous assignment's task and its own event then mints the new assignee's.
// The old task's conversation is still held by a live claim when the mint
// runs, which is the state a sibling event's close reopens from, so the task
// closes as reassigned and the new assignee's team gets a task of its own.
func TestReopen_ReassignmentLeavesTheOldTaskClosed(t *testing.T) {
	database, router, _, teamB, entityID := reassignFixture(t)
	router.spawner = &stubDelegator{}
	mustHandle(t, router, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-alice"))
	first := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(first) != 1 {
		t.Fatalf("after the first assignment: %d tasks, want 1", len(first))
	}
	_, convID := seedRunOnTask(t, database, first[0].ID, "running", "")
	if _, err := database.Exec(`
		INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, lease_expires_at)
		VALUES (?, ?, ?, 'reopen-test', 1, strftime('%Y-%m-%d %H:%M:%f', 'now', '+300 seconds'))
	`, uuid.New().String(), runmode.LocalDefaultOrgID, convID); err != nil {
		t.Fatalf("seed the working claim: %v", err)
	}

	mustHandle(t, router, linearEvent(domain.EventLinearIssueAssigned, entityID, "lt-eng", "lu-bob"))

	if status, reason, eventType := taskRow(t, database, first[0].ID); status != "done" || reason != dbpkg.ReassignedCloseReason || eventType != domain.EventLinearIssueAssigned {
		t.Errorf("the old assignment's task = (%q, %q, %q), want done as reassigned by the assignment", status, reason, eventType)
	}
	active := activeOfType(t, database, entityID, domain.EventLinearIssueAssigned)
	if len(active) != 1 || active[0].ID == first[0].ID || teamIDValue(&active[0]) != teamB {
		t.Errorf("active assigned tasks = %+v, want one new task owned by team B", active)
	}
}
