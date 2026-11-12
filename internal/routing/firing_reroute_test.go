package routing

import (
	"context"
	"database/sql"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// rerouteScenario routes a CI failure for a one-team org with an immediate
// trigger, so the task exists and its run has fired, then records a second
// failure the way the tracker would. It returns the entity, the first task
// and the second event's id.
func rerouteScenario(t *testing.T, database *sql.DB, router *Router) (entityID, taskID, secondEventID string) {
	t.Helper()
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	team := seedTeam(t, database, "team-a")
	seedUserOnTeam(t, database, team, "aidan")
	enableTeamAutoDelegate(t, database, team)
	seedImmediateTrigger(t, database, team, domain.EventGitHubPRCICheckFailed, "ci-a")

	entityID = reviewEntity(t, database, "owner/repo#reroute")
	emitCI(t, router, entityID, "aidan")
	tasks, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 CI task, got %d (err=%v)", len(tasks), err)
	}
	taskID = tasks[0].ID

	secondEventID, err = sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, ciFailedEvent(entityID, "aidan"))
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}
	return entityID, taskID, secondEventID
}

// finishRunClosingTask ends the task's run and closes the task the way a
// blueprint's finish does.
func finishRunClosingTask(t *testing.T, database *sql.DB, taskID string) {
	t.Helper()
	endTaskConversations(t, database, taskID)
	if _, err := testTaskStore(database).Close(t.Context(), runmode.LocalDefaultOrgID, taskID, "run_completed", ""); err != nil {
		t.Fatalf("close task: %v", err)
	}
}

// An event queued behind a run whose finish closed the task is routed again:
// a new task is minted for it and that task's run starts.
func TestFiringWorker_RunFinishClosedTask_ReroutesTheEvent(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, taskID, eventID := rerouteScenario(t, database, router)
	if stub.calls != 1 {
		t.Fatalf("Delegate called %d times for the first event, want 1", stub.calls)
	}

	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", eventID)
	finishRunClosingTask(t, database, taskID)
	drainOnce(t, router)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipRerouted)

	active, err := testTaskStore(database).FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active CI task after the reroute, got %d (err=%v)", len(active), err)
	}
	if active[0].ID == taskID {
		t.Fatalf("the reroute left the closed task as the active one")
	}
	if stub.calls != 2 {
		t.Errorf("Delegate called %d times, want 2: the new task's run did not start", stub.calls)
	}
	var runEvent string
	if err := database.QueryRow(`SELECT triggering_event_id FROM blueprint_runs WHERE task_id = ?`, active[0].ID).Scan(&runEvent); err != nil {
		t.Fatalf("read the new task's run: %v", err)
	}
	if runEvent != eventID {
		t.Errorf("the new task's run fired from event %s, want the rerouted event %s", runEvent, eventID)
	}
}

// One event queues a firing per matched trigger. Routing it again once decides
// every trigger, so the second firing settles without routing the event a
// second time.
func TestFiringWorker_Reroute_RoutesAnEventOncePerTask(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, taskID, eventID := rerouteScenario(t, database, router)

	var team string
	if err := database.QueryRow(`SELECT team_id FROM event_handlers WHERE id = 'trig-ci-a'`).Scan(&team); err != nil {
		t.Fatalf("read trigger team: %v", err)
	}
	seedImmediateTrigger(t, database, team, domain.EventGitHubPRCICheckFailed, "ci-b")
	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", eventID)
	enqueueFiring(t, database, entityID, taskID, "trig-ci-b", eventID)
	finishRunClosingTask(t, database, taskID)
	drainOnce(t, router)

	rerouted, queuedOnNew := 0, 0
	for _, f := range firingsFor(t, database, entityID) {
		switch {
		case f.TaskID == taskID:
			requireSkipped(t, f, domain.PendingFiringSkipRerouted)
			rerouted++
		case f.Status == workitem.StatusReady:
			queuedOnNew++
		}
	}
	if rerouted != 2 {
		t.Errorf("%d firings on the closed task settled rerouted, want 2", rerouted)
	}
	// The one routing fires the first trigger on the new task and queues the
	// second behind its run. A second routing would queue the first as well.
	if queuedOnNew != 1 {
		t.Errorf("%d firings queued on the new task, want 1: the event was routed more than once", queuedOnNew)
	}
	if stub.calls != 2 {
		t.Errorf("Delegate called %d times, want 2", stub.calls)
	}
}

// A close that answers the event is not a run finishing: the firing settles
// as it did, and nothing is routed.
func TestFiringWorker_EventClosedTask_IsNotRerouted(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, taskID, eventID := rerouteScenario(t, database, router)

	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", eventID)
	endTaskConversations(t, database, taskID)
	if _, err := testTaskStore(database).Close(t.Context(), runmode.LocalDefaultOrgID, taskID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed); err != nil {
		t.Fatalf("close task: %v", err)
	}
	drainOnce(t, router)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipTaskClosed)
	if stub.calls != 1 {
		t.Errorf("Delegate called %d times, want 1: an event-closed task's firing started a run", stub.calls)
	}
}
