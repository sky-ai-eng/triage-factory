package routing

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

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

// dispositionCounter counts the routing dispositions a router publishes.
type dispositionCounter struct {
	mu sync.Mutex
	n  int
}

func (c *dispositionCounter) Publish(evt domain.Event) {
	if evt.EventType != domain.EventSystemRoutingDisposition {
		return
	}
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *dispositionCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A firing names the first event queued for its (task, trigger); a later one
// collapsed onto the row. The reroute routes the task's newest event, so the
// new task and its run are about the situation as it last stood.
func TestFiringWorker_Reroute_RoutesTheTasksNewestEvent(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, taskID, queuedEventID := rerouteScenario(t, database, router)
	st := sqlitestore.New(database)
	tasks := testTaskStore(database)

	bump := func(eventID string) {
		t.Helper()
		if err := tasks.RecordEventSystem(t.Context(), runmode.LocalDefaultOrgID, taskID, eventID, "bumped"); err != nil {
			t.Fatalf("record bump: %v", err)
		}
	}
	bump(queuedEventID)
	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", queuedEventID)
	newer := ciFailedEvent(entityID, "aidan")
	newer.CreatedAt = time.Now().Add(time.Minute)
	newestEventID, err := st.Events.Record(context.Background(), runmode.LocalDefaultOrgID, newer)
	if err != nil {
		t.Fatalf("record newest event: %v", err)
	}
	bump(newestEventID)
	// The second admission collapses onto the first firing's row.
	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", newestEventID)

	finishRunClosingTask(t, database, taskID)
	drainOnce(t, router)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 || rows[0].TriggeringEventID != queuedEventID {
		t.Fatalf("firings = %+v, want the one row naming the first event", rows)
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipRerouted)
	active, err := tasks.FindActiveByEntityAndType(context.Background(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active CI task after the reroute, got %d (err=%v)", len(active), err)
	}
	var runEvent string
	if err := database.QueryRow(`SELECT triggering_event_id FROM blueprint_runs WHERE task_id = ?`, active[0].ID).Scan(&runEvent); err != nil {
		t.Fatalf("read the new task's run: %v", err)
	}
	if runEvent != newestEventID {
		t.Errorf("the new task's run fired from event %s, want the task's newest event %s", runEvent, newestEventID)
	}
}

// The event's first routing published its disposition, which subscribers act
// on once. Routing it again publishes none.
func TestFiringWorker_Reroute_PublishesNoSecondDisposition(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	pub := &dispositionCounter{}
	deps := firingRouterDeps(database, stub)
	deps.Publisher = pub
	router := newTestRouter(deps)
	entityID, taskID, eventID := rerouteScenario(t, database, router)

	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", eventID)
	finishRunClosingTask(t, database, taskID)

	published := pub.count()
	if published == 0 {
		t.Fatalf("setup: the first routing published no disposition")
	}
	drainOnce(t, router)

	requireSkipped(t, firingsFor(t, database, entityID)[0], domain.PendingFiringSkipRerouted)
	if stub.calls != 2 {
		t.Fatalf("Delegate called %d times, want 2: the reroute did not start the new task's run", stub.calls)
	}
	if got := pub.count(); got != published {
		t.Errorf("the reroute published %d routing dispositions, want none", got-published)
	}
}

// A terminating event closed its entity when it first routed. Routed again it
// mints nothing, whether the entity is still closed or has reopened.
func TestRouteEvent_Again_ATerminatingEventRoutesNothing(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, _, _ := rerouteScenario(t, database, router)

	merged := domain.Event{
		EventType: domain.EventGitHubPRMerged, EntityID: &entityID,
		MetadataJSON: `{"author":"aidan","repo":"owner/repo"}`, OrgID: runmode.LocalDefaultOrgID,
	}
	if err := router.routeEvent(t.Context(), merged, nil, true); err != nil {
		t.Fatalf("routeEvent again: %v", err)
	}
	entity, err := sqlitestore.New(database).Entities.GetSystem(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || entity == nil {
		t.Fatalf("read entity: %v", err)
	}
	if entity.State != "active" {
		t.Errorf("entity state = %q, want it left active: a second routing closed it", entity.State)
	}
}

// handOffNote stages a note on the task's conversation and marks the task's
// one ready firing with it, as an engagement's start does.
func handOffNote(t *testing.T, database *sql.DB, entityID, taskID string) (noteID, conversationID string) {
	t.Helper()
	st := sqlitestore.New(database)
	if err := database.QueryRow(`SELECT id FROM conversations WHERE task_id = ?`, taskID).Scan(&conversationID); err != nil {
		t.Fatalf("read the task's conversation: %v", err)
	}
	noteID, handed, err := st.PendingFirings.HandOffToConversation(t.Context(), runmode.LocalDefaultOrgID, firingsFor(t, database, entityID)[0].ID,
		&domain.StagedInjection{ConversationID: conversationID, Subtype: domain.MessageSubtypeInjectionEvent, Producer: "p", Body: "a new event"}, "")
	if err != nil || !handed {
		t.Fatalf("hand off: handed=%v err=%v", handed, err)
	}
	return noteID, conversationID
}

// A firing whose event was handed to a step as a note, and read, is settled
// injected once the run ends: no second run, and no reroute.
func TestFiringWorker_HandedNoteRead_SettlesInjected(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, taskID, eventID := rerouteScenario(t, database, router)
	if err := testTaskStore(database).RecordEventSystem(t.Context(), runmode.LocalDefaultOrgID, taskID, eventID, "bumped"); err != nil {
		t.Fatalf("record bump: %v", err)
	}
	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", eventID)
	_, conversationID := handOffNote(t, database, entityID, taskID)
	if _, err := sqlitestore.New(database).StagedInjections.FlushPendingSystem(t.Context(), runmode.LocalDefaultOrgID, conversationID); err != nil {
		t.Fatalf("flush: %v", err)
	}

	finishRunClosingTask(t, database, taskID)
	drainOnce(t, router)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipInjected)
	if stub.calls != 1 {
		t.Errorf("Delegate called %d times, want 1: a read note started another run", stub.calls)
	}
	var kind string
	if err := database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, taskID, eventID).Scan(&kind); err != nil || kind != "injected" {
		t.Errorf("task event kind = %q (err=%v), want injected", kind, err)
	}
}

// A firing whose note was never read (the step it was staged for did not
// reach its agent) is an ordinary firing again: the note is withdrawn and,
// with the task still open, a fresh run fires.
func TestFiringWorker_HandedNoteUnread_WithdrawsTheNoteAndFires(t *testing.T) {
	database := newTestDB(t)
	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	entityID, taskID, eventID := rerouteScenario(t, database, router)
	enqueueFiring(t, database, entityID, taskID, "trig-ci-a", eventID)
	_, conversationID := handOffNote(t, database, entityID, taskID)

	// The run ends without the step having read the note, and the task
	// stays open.
	endTaskConversations(t, database, taskID)
	drainOnce(t, router)

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireFired(t, rows[0])
	if stub.calls != 2 {
		t.Errorf("Delegate called %d times, want 2: the unread event started no run", stub.calls)
	}
	notes, err := sqlitestore.New(database).StagedInjections.FlushPendingSystem(t.Context(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil || len(notes) != 0 {
		t.Errorf("staged notes after the worker = %+v (err=%v), want the note withdrawn", notes, err)
	}
}
