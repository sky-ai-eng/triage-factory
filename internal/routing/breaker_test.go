package routing

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
	"github.com/sky-ai-eng/triage-factory/internal/notify/notifytest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// breakerBase is when the first seeded run started; later runs start a minute
// apart, so the streak's order never rests on two rows sharing a timestamp.
var breakerBase = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// seedFinishedRun stages a blueprint run of blueprintID on taskID, finished
// in status, started minute minutes after breakerBase, with one step
// conversation, and returns the conversation's id. triggerType is "event"
// for a run a trigger fired and "manual" for one a person started.
func seedFinishedRun(t *testing.T, database *sql.DB, taskID, blueprintID, triggerType, status string, minute int) string {
	t.Helper()
	steps, err := sqlitestore.New(database).Blueprints.ListSteps(context.Background(), runmode.LocalDefaultOrgID, blueprintID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("blueprint %s steps: %v (%d)", blueprintID, err, len(steps))
	}
	startedAt := breakerBase.Add(time.Duration(minute) * time.Minute)
	var creator any
	if triggerType == "manual" {
		creator = runmode.LocalDefaultUserID
	}
	runID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, creator_user_id,
		                            status, worktree_path, step_plan, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '[]', ?)
	`, runID, blueprintID, taskID, triggerType, creator, status, "/tmp/wt-"+runID, startedAt); err != nil {
		t.Fatalf("seed %s %s run: %v", triggerType, status, err)
	}
	conversationID := uuid.New().String()
	step := 0
	dbtest.SeedConversation(t, database, domain.Conversation{
		ID: conversationID, TaskID: taskID, PromptID: steps[0].StepPromptID, TriggerType: triggerType,
		BlueprintRunID: runID, BlueprintStepIndex: &step, StartedAt: startedAt,
	})
	// The run is finished, so its conversation has ended: a live one would
	// hold the task, and the firing worker would wait on it.
	end := `UPDATE conversations SET status = 'failed', ended_at = CURRENT_TIMESTAMP, ended_reason = 'failed' WHERE id = ?`
	if status == "completed" {
		end = `UPDATE conversations SET status = 'open', completed_at = CURRENT_TIMESTAMP,
		       ended_at = CURRENT_TIMESTAMP, ended_reason = 'step_advanced' WHERE id = ?`
	}
	if _, err := database.Exec(end, conversationID); err != nil {
		t.Fatalf("end the run's conversation: %v", err)
	}
	return conversationID
}

// trippedTrigger stands up the drain scenario with its trigger's four failed
// runs on the entity, enough for the threshold of 4 it carries, and returns
// the trigger and the conversation of the run that failed last.
func trippedTrigger(t *testing.T, database *sql.DB) (entityID, taskID, eventID string, trigger domain.EventHandler, lastConversationID string) {
	t.Helper()
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	got, err := sqlitestore.New(database).EventHandlers.GetSystem(context.Background(), runmode.LocalDefaultOrgID, triggerID)
	if err != nil || got == nil {
		t.Fatalf("read trigger: %v", err)
	}
	trigger = *got
	// A breaker keyed on the step prompt would count these runs only if the
	// two shared an id, so the fixture keeps them apart.
	if trigger.BlueprintID == "p-drain-1" {
		t.Fatalf("the trigger's blueprint shares its step prompt's id; the test needs them apart")
	}
	for i := range 4 {
		lastConversationID = seedFinishedRun(t, database, taskID, trigger.BlueprintID, "event", "failed", i)
	}
	return entityID, taskID, eventID, trigger, lastConversationID
}

func breakerKey(triggerID, entityID string) string { return triggerID + ":" + entityID }

// TestBreaker_TheTriggersFailedRunsTripItAndTellItsTeam: four failed runs of
// a trigger with a threshold of 4 stop its queued firing, and the trigger's
// team is told which entity, how many runs failed and which run failed last.
func TestBreaker_TheTriggersFailedRunsTripItAndTellItsTeam(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	entityID, taskID, eventID, trigger, last := trippedTrigger(t, database)
	enqueueFiring(t, database, entityID, taskID, trigger.ID, eventID)
	stub := &stubDelegator{db: database}
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, stub))

	rows := firingsFor(t, database, entityID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 firing row, got %d", len(rows))
	}
	requireSkipped(t, rows[0], domain.PendingFiringSkipBreakerTripped)
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times past a tripped breaker", stub.calls)
	}

	open := rec.Open(runmode.LocalDefaultOrgID, kinds.BreakerTripped)
	if len(open) != 1 || open[0].Key != breakerKey(trigger.ID, entityID) {
		t.Fatalf("open = %+v, want the trigger's breaker on the entity", open)
	}
	b := open[0].Params.(kinds.Breaker)
	want := kinds.Breaker{
		TriggerID: trigger.ID, EntityID: entityID, TeamID: runmode.LocalDefaultTeamID,
		EventType: domain.EventGitHubPRCICheckFailed, Subject: "owner/repo#1", Failures: 4, ConversationID: last,
	}
	if b != want {
		t.Errorf("raised %+v, want %+v", b, want)
	}
}

// TestBreaker_BelowTheThresholdNothingTrips: three failed runs under a
// threshold of 4 fire the queued firing and tell nobody.
func TestBreaker_BelowTheThresholdNothingTrips(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	entityID, taskID, triggerID, eventID := setupDrainScenario(t, database)
	trigger, err := sqlitestore.New(database).EventHandlers.GetSystem(context.Background(), runmode.LocalDefaultOrgID, triggerID)
	if err != nil || trigger == nil {
		t.Fatalf("read trigger: %v", err)
	}
	for i := range 3 {
		seedFinishedRun(t, database, taskID, trigger.BlueprintID, "event", "failed", i)
	}
	enqueueFiring(t, database, entityID, taskID, triggerID, eventID)
	drainOnce(t, drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database}))

	requireFired(t, firingsFor(t, database, entityID)[0])
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)
}

// TestBreaker_TheEventTimeGateTellsTheTeamToo: an event-time firing on a task
// with nothing live on it is refused at the breaker, starts no run, and raises
// the same notification the firing worker does.
func TestBreaker_TheEventTimeGateTellsTheTeamToo(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	seedLocalBot(t, database)
	entityID, taskID, eventID, trigger, _ := trippedTrigger(t, database)
	stub := &stubDelegator{db: database}
	r := drainRouter(database, sqlitestore.New(database).PendingFirings, stub)
	task, err := sqlitestore.New(database).Tasks.GetSystem(context.Background(), runmode.LocalDefaultOrgID, taskID)
	if err != nil || task == nil {
		t.Fatalf("read task: %v", err)
	}

	fired, err := r.tryAutoDelegate(context.Background(), runmode.LocalDefaultOrgID, task, trigger, entityID, eventID, runmode.LocalDefaultTeamID)
	if err != nil || fired {
		t.Fatalf("tryAutoDelegate = (fired %v, %v), want a decided skip", fired, err)
	}
	if stub.calls != 0 {
		t.Errorf("Delegate called %d times past a tripped breaker", stub.calls)
	}
	if rows := firingsFor(t, database, entityID); len(rows) != 0 {
		t.Errorf("queued %d firings past a tripped breaker, want none", len(rows))
	}
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped, breakerKey(trigger.ID, entityID))
}

// TestBreaker_AnEventStillReachesTheTasksLiveConversation: a tripped breaker
// bars new runs, not an event folded into the conversation already live on
// the task — often the person's own run that would reset it.
func TestBreaker_AnEventStillReachesTheTasksLiveConversation(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	entityID, task, trigger, activeConversationID := setupAbsorbScenario(t, database)
	for i := range 4 {
		seedFinishedRun(t, database, task.ID, trigger.BlueprintID, "event", "failed", i)
	}
	streak, err := sqlitestore.New(database).Tasks.BreakerStreakSystem(context.Background(), runmode.LocalDefaultOrgID, entityID, trigger.BlueprintID)
	if err != nil || streak.Failures < derefIntDefault(trigger.BreakerThreshold, 0) {
		t.Fatalf("streak = %+v (%v), want the breaker holding for threshold %v", streak, err, trigger.BreakerThreshold)
	}
	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: absorbTestEventType, EntityID: &entityID, MetadataJSON: `{"mention":"second"}`,
		CreatedAt: time.Now(), OrgID: runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}
	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal}
	router := newAbsorbTestRouter(database, sqlitestore.New(database).Conversations, stub)
	bumpTaskViaRealUpsert(t, router, task, trigger, entityID, secondEventID)

	injected := ""
	if fired, err := router.tryAutoDelegateTrackingInjection(context.Background(), runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "", &injected); err != nil || fired {
		t.Fatalf("tryAutoDelegateTrackingInjection = (fired %v, %v), want an injection", fired, err)
	}
	if injected != activeConversationID || len(stub.calls) != 1 {
		t.Errorf("injected into %q with %d calls, want the live conversation %q once", injected, len(stub.calls), activeConversationID)
	}
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)
}

// TestBreaker_APersonsCompletedRunResetsIt: the sync keeps the notification
// while the breaker holds, resolves it once a run a person started on the
// entity completes, and the trigger's next firing fires.
func TestBreaker_APersonsCompletedRunResetsIt(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	entityID, taskID, eventID, trigger, _ := trippedTrigger(t, database)
	r := drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database})
	enqueueFiring(t, database, entityID, taskID, trigger.ID, eventID)
	drainOnce(t, r)
	key := breakerKey(trigger.ID, entityID)
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped, key)

	r.syncBreakers(context.Background())
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped, key)

	// A person's run that failed resets nothing.
	seedFinishedRun(t, database, taskID, trigger.BlueprintID, "manual", "failed", 10)
	r.syncBreakers(context.Background())
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped, key)

	seedFinishedRun(t, database, taskID, trigger.BlueprintID, "manual", "completed", 11)
	r.syncBreakers(context.Background())
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)

	secondEvent, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, EntityID: &entityID, DedupKey: "build", MetadataJSON: `{"check_name":"build"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	enqueueFiring(t, database, entityID, taskID, trigger.ID, secondEvent)
	drainOnce(t, r)
	rows := firingsFor(t, database, entityID)
	requireFired(t, rows[len(rows)-1])
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)
}

// TestBreaker_TheSyncResolvesWhatTheGateNeverSees: turning the trigger off
// fires nothing that would reach the gate, so the sync is what resolves it.
func TestBreaker_TheSyncResolvesWhatTheGateNeverSees(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	entityID, taskID, eventID, trigger, _ := trippedTrigger(t, database)
	r := drainRouter(database, sqlitestore.New(database).PendingFirings, &stubDelegator{db: database})
	enqueueFiring(t, database, entityID, taskID, trigger.ID, eventID)
	drainOnce(t, r)
	rec.Active(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)

	setTriggerEnabledForTestRouting(t, database, trigger.ID, false)
	r.syncBreakers(context.Background())
	rec.Inactive(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)
}

// TestBreaker_ABreakerThatTripsDuringAPassIsOpened: the pass's second read
// opens a breaker its first read did not see, as a gate that refused between
// the read and the sync would have, with the occurrence the gate raises.
func TestBreaker_ABreakerThatTripsDuringAPassIsOpened(t *testing.T) {
	database := newTestDB(t)
	rec := notifytest.Record(t)
	entityID, _, _, trigger, last := trippedTrigger(t, database)
	r := newTestRouter(testRouterDeps(database))

	r.reopenTrippedDuring(context.Background(), runmode.LocalDefaultOrgID, nil)
	open := rec.Open(runmode.LocalDefaultOrgID, kinds.BreakerTripped)
	if len(open) != 1 {
		t.Fatalf("open = %+v, want the breaker the first read did not see", open)
	}
	if b := open[0].Params.(kinds.Breaker); b.Subject != "owner/repo#1" || b.Failures != 4 || b.ConversationID != last {
		t.Errorf("raised %+v, want the entity's key, 4 failures and the last failed run", b)
	}

	// One the first read saw is left to the sync.
	before := []domain.TrippedBreaker{{TriggerID: trigger.ID, EntityID: entityID}}
	rec2 := notifytest.Record(t)
	r.reopenTrippedDuring(context.Background(), runmode.LocalDefaultOrgID, before)
	rec2.Inactive(t, runmode.LocalDefaultOrgID, kinds.BreakerTripped)
}
