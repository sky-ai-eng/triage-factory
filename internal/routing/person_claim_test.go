package routing

import (
	"sync/atomic"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPersonClaim_OwnEventStartsNoRun: a person holds the claim on an open
// task with nothing working on it, and the task's own event arrives again (CI
// fails on the person's next push). The team's trigger matches, and the event
// is recorded on the task, but it starts no run and queues no firing: the
// task stays the person's. Once the person returns the task to the queue, the
// same event starts a run, so the claim is what held it.
func TestPersonClaim_OwnEventStartsNoRun(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedImmediateTrigger(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, "ci-person-claim")

	stub := &stubDelegator{db: database}
	pub := &fakeDispositionPublisher{}
	deps := testRouterDeps(database)
	deps.Spawner = stub
	deps.Publisher = pub
	router := newTestRouter(deps)
	entityID := reviewEntity(t, database, "owner/repo#person-claimed")

	// The first failure creates the task before the team has an agent, so
	// the task is open with no run. The agent is attached before the
	// recurrence, which leaves the person's claim as the only thing between
	// the trigger and a run.
	emitCI(t, router, entityID, "aidan")
	active := activeCIFailedTasks(t, database, entityID)
	if len(active) != 1 {
		t.Fatalf("setup: active ci tasks = %+v, want one", active)
	}
	taskID := active[0].ID
	if n := atomic.LoadInt64(&stub.calls); n != 0 {
		t.Fatalf("setup: delegations = %d, want none before the team has an agent", n)
	}
	enableTeamAutoDelegate(t, database, runmode.LocalDefaultTeamID)
	tasks := testTaskStore(database)
	if ok, err := tasks.ClaimQueuedForUser(t.Context(), runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultUserID, dbtest.TaskVersion(t, tasks, runmode.LocalDefaultOrgID, taskID)); err != nil || !ok {
		t.Fatalf("setup: claim the task for a person: ok=%v err=%v", ok, err)
	}

	emitCI(t, router, entityID, "aidan")

	published := pub.eventsCopy()
	if len(published) == 0 {
		t.Fatal("no routing disposition published")
	}
	disp := decodeDisposition(t, published[len(published)-1].MetadataJSON)
	if disp.Disposition != events.DispositionTaskBumped || disp.TaskID != taskID || disp.TriggersFired != 0 {
		t.Errorf("disposition = %q on task %q with %d triggers fired, want %q on %q with none",
			disp.Disposition, disp.TaskID, disp.TriggersFired, events.DispositionTaskBumped, taskID)
	}
	var kind string
	if err := database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, taskID, disp.EventID).Scan(&kind); err != nil {
		t.Fatalf("read the recurrence's task_event: %v", err)
	}
	if kind != "bumped" {
		t.Errorf("task_events.kind for the recurrence = %q, want bumped", kind)
	}

	var runs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM blueprint_runs WHERE task_id = ?`, taskID).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("blueprint runs on the task = %d, want none on a task a person holds", runs)
	}
	if n := atomic.LoadInt64(&stub.calls); n != 0 {
		t.Errorf("delegations = %d, want none on a task a person holds", n)
	}
	firings, err := sqlitestore.New(database).PendingFirings.ListForEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(firings) != 0 {
		t.Errorf("pending firings = %+v, want none on a task a person holds", firings)
	}
	task, err := tasks.Get(t.Context(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("re-read task: %v", err)
	}
	if task.ClaimedByUserID != runmode.LocalDefaultUserID || task.ClaimedByAgentID != "" {
		t.Errorf("claim = (agent=%q, user=%q), want the person's claim to stand", task.ClaimedByAgentID, task.ClaimedByUserID)
	}

	if ok, err := tasks.RequeueTask(t.Context(), runmode.LocalDefaultOrgID, taskID, task.Version); err != nil || !ok {
		t.Fatalf("return the task to the queue: ok=%v err=%v", ok, err)
	}
	emitCI(t, router, entityID, "aidan")
	if n := atomic.LoadInt64(&stub.calls); n != 1 {
		t.Errorf("delegations after the requeue = %d, want the trigger to start a run on the unclaimed task", n)
	}
}
