package routing

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Universal same-task absorption: a trigger firing whose OWN task already
// holds the entity's live auto run folds into that run (inject + claim);
// every cross-task firing defers to pending_firings exactly as before.
// These tests exercise that gate through the retained injection delivery
// seam (StageOrDeliverAdditiveEvent / AdditiveFiringRef).

// absorbTestEventType is a synthetic event type these tests register with a
// match-all predicate, mirroring fakeEventType's out-of-core-source pattern
// (source_registry_test.go) rather than mutating a real registered type
// (e.g. ci_check_failed) for the duration of a test. Registered per-test
// (not via init()) and cleaned up with t.Cleanup(events.Reset(...)), so it
// never leaks into sibling tests. absorbOtherEventType is a second type used
// by the cross-task case, so its task is a different task on the same entity.
const (
	absorbTestEventType  = "fake:absorption:mentioned"
	absorbOtherEventType = "fake:absorption:other"
)

// registerAbsorbEventType registers eventType with a match-all predicate and
// schedules its removal from the registry at test end. Absorption is decided
// by same-task identity now, not a per-type flag, so no schema field is set.
func registerAbsorbEventType(t *testing.T, eventType string) {
	t.Helper()
	events.Register(events.EventSchema{
		EventType: eventType,
		Ownership: events.OwnershipOwned,
		Match: func(predJSON, metaJSON string) (bool, error) {
			return true, nil
		},
	})
	t.Cleanup(func() { events.Reset(eventType) })
}

// seedAbsorbEventCatalog inserts the events_catalog row eventType needs to
// satisfy the event_handlers.event_type / events.event_type /
// tasks.event_type FKs — mirrors seedFakeEventCatalog in
// source_registry_test.go.
func seedAbsorbEventCatalog(t *testing.T, database *sql.DB, eventType string) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT OR IGNORE INTO events_catalog (id, source, category, label, description)
		VALUES (?, 'fake', 'absorption', ?, 'same-task absorption test fixture')
	`, eventType, eventType); err != nil {
		t.Fatalf("seed events_catalog for %s: %v", eventType, err)
	}
}

// injectingStubDelegator is a Delegator fake for the absorption gate tests:
// it never fires a new run (the busy-entity branch under test never calls
// Delegate) and records every StageOrDeliverAdditiveEvent call (including the
// firing ref) so tests can assert what got routed, where. outcome controls
// the return value — the gate's dispatch logic is what these tests exercise;
// StageOrDeliverAdditiveEvent's own internal decision-making
// (local/remote/staged/resumable) is covered directly in internal/delegate.
type injectingStubDelegator struct {
	outcome delegate.InjectOutcome
	calls   []injectCall
	// allowFire lets a test exercise the OPEN-gate branch: a firing on a
	// task with no live run of its own fires normally, even while a sibling
	// task on the same entity is busy.
	allowFire bool
	delegated []string
}

type injectCall struct {
	orgID, runID, producer, body string
	firing                       delegate.AdditiveFiringRef
}

func (s *injectingStubDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	if !s.allowFire {
		return "", fmt.Errorf("unexpected Delegate call: this test only exercises the busy-task branch")
	}
	s.delegated = append(s.delegated, task.ID)
	return "run-" + task.ID, nil
}

func (s *injectingStubDelegator) Cancel(orgID, runID, userID string) error { return nil }

func (s *injectingStubDelegator) StageOrDeliverAdditiveEvent(ctx context.Context, orgID, runID, producer, body string, firing delegate.AdditiveFiringRef) delegate.InjectOutcome {
	s.calls = append(s.calls, injectCall{orgID, runID, producer, body, firing})
	return s.outcome
}

// setupAbsorbScenario seeds entity + prompt + a trigger on
// absorbTestEventType + an initial event, then delegates it into an active
// run (mirroring a real in-flight auto run) via stubDelegateRun. Returns the
// entity, the task the trigger matches (which owns the active run), the
// trigger (with its real resolved blueprint id), and the active run's ID.
func setupAbsorbScenario(t *testing.T, database *sql.DB) (entityID string, task *domain.Task, trigger domain.EventHandler, activeRunID string) {
	t.Helper()
	registerAbsorbEventType(t, absorbTestEventType)
	seedAbsorbEventCatalog(t, database, absorbTestEventType)

	st := sqlitestore.New(database)
	entity, _, err := st.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "fake", "fake/thing#"+uuid.New().String()[:8], "thing",
		"Absorption Thing", "https://example.com/absorption")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID = entity.ID

	createTestPrompt(t, database, domain.Prompt{ID: "p-absorb", Name: "Absorb", Body: "x", Source: "user"})

	firstEventID, err := st.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"first":true}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record first event: %v", err)
	}

	gotTask, _, err := testTaskStore(database).FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		entityID, absorbTestEventType, "", firstEventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task = gotTask

	trig := domain.EventHandler{
		ID:                     "trigger-absorb",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-absorb",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              absorbTestEventType,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trig)
	if err := database.QueryRow(`SELECT blueprint_id FROM event_handlers WHERE id = ?`, trig.ID).Scan(&trig.BlueprintID); err != nil {
		t.Fatalf("resolve trigger blueprint id: %v", err)
	}
	trigger = trig

	// Seed the "already active" run the busy-entity gate will see —
	// mirrors the immediate-fire path's own bookkeeping (a fenced
	// blueprint_run + a running event-triggered run row) without going
	// through the full HandleEvent dispatch.
	blueprintRunID, err := stubDelegateRun(database, *task, delegate.DelegateOpts{
		ExplicitBlueprintID: trigger.BlueprintID,
		TriggerType:         "event",
		TriggerID:           trigger.ID,
		TriggeringEventID:   firstEventID,
	})
	if err != nil {
		t.Fatalf("seed active run: %v", err)
	}
	// stubDelegateRun returns the blueprint_run id, not the runs.id the
	// injection seam targets (staged_agent_injections.run_id FKs to
	// runs(id)) — resolve the real run row it minted (one run per
	// blueprint_run in the stub, step_index 0).
	if err := database.QueryRow(`SELECT id FROM conversations WHERE blueprint_run_id = ?`, blueprintRunID).Scan(&activeRunID); err != nil {
		t.Fatalf("resolve seeded run id: %v", err)
	}
	return
}

// bumpTaskViaRealUpsert routes eventID through the router's real
// upsertTaskForEvent — the exact write production performs (BumpSystem +
// RecordEventSystem(..., "bumped")) for a second event landing on an
// already-active task — BEFORE the test goes on to exercise the injection
// branch. Without this, a masking test's "second event" never occupied
// (task.ID, eventID) in task_events at all, so MarkEventInjectedSystem's
// UPDATE both looks like it works even if the real collision it's meant to
// resolve was never reproduced. A state production cannot produce (the event
// that triggers injection is exactly the event that bumped the task) must
// exist before injection runs.
func bumpTaskViaRealUpsert(t *testing.T, router *Router, task *domain.Task, trigger domain.EventHandler, entityID, eventID string) {
	t.Helper()
	routing := eventRouting{
		ownerTeam:    runmode.LocalDefaultTeamID,
		visibleTeams: []string{runmode.LocalDefaultTeamID},
		orderedTeams: []string{runmode.LocalDefaultTeamID},
		teamTriggers: map[string][]domain.EventHandler{runmode.LocalDefaultTeamID: {trigger}},
		taskPriority: 0.5,
	}
	bumped, created, err := router.upsertTaskForEvent(runmode.LocalDefaultOrgID, domain.Event{EventType: absorbTestEventType, ID: eventID}, entityID, routing)
	if err != nil {
		t.Fatalf("upsertTaskForEvent (bump): %v", err)
	}
	if created || bumped == nil || bumped.ID != task.ID {
		t.Fatalf("expected the second event to bump the existing task %q, got created=%v task=%+v", task.ID, created, bumped)
	}
}

// newAbsorbTestRouter builds a Router wired for the absorption gate tests,
// sharing the same store construction every test below needs.
func newAbsorbTestRouter(database *sql.DB, agentRuns db.ConversationStore, stub *injectingStubDelegator) *Router {
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), agentRuns, st.Entities, st.PendingFirings,
		st.Events, st.Orgs, st.Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())
}

// TestTryAutoDelegate_SameTask_InjectsIntoActiveRun is the ticket's primary
// acceptance: a firing whose own task already holds the entity's active auto
// run injects into that run (StageOrDeliverAdditiveEvent reporting
// InjectDeliveredLocal) instead of deferring — no pending_firings row lands,
// and the task_events row records the injection.
func TestTryAutoDelegate_SameTask_InjectsIntoActiveRun(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAbsorbScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal}
	router := newAbsorbTestRouter(database, sqlitestore.New(database).Conversations, stub)
	// Reproduce the real collision: production's own upsertTaskForEvent
	// already wrote a "bumped" task_events row for (task.ID, secondEventID)
	// before tryAutoDelegate/tryAdditiveInjection ever runs.
	bumpTaskViaRealUpsert(t, router, task, trigger, entityID, secondEventID)

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected exactly 1 StageOrDeliverAdditiveEvent call, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	if got.runID != activeRunID {
		t.Errorf("injected run_id = %q, want the active run %q", got.runID, activeRunID)
	}
	if got.producer != absorbTestEventType {
		t.Errorf("injected producer = %q, want %q", got.producer, absorbTestEventType)
	}
	if got.body == "" {
		t.Error("injected body is empty")
	}
	if got.firing.EntityID != entityID || got.firing.TaskID != task.ID || got.firing.TriggerID != trigger.ID || got.firing.TriggeringEventID != secondEventID {
		t.Errorf("firing ref not populated correctly: %+v", got.firing)
	}

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no pending_firings row (injected, not deferred), got %d", len(rows))
	}

	var kind string
	if err := database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, task.ID, secondEventID).Scan(&kind); err != nil {
		t.Fatalf("read task_events for injected bookkeeping: %v", err)
	}
	if kind != "injected" {
		t.Errorf("task_events.kind = %q, want %q", kind, "injected")
	}
}

// TestTryAutoDelegate_SameTask_StagedResumableRunHandled is the success
// counterpart above for the "durably staged onto a resumable run" outcome —
// StageOrDeliverAdditiveEvent reporting InjectStagedResumable must be
// treated as handled exactly like InjectDeliveredLocal: task_events
// recorded, no pending_firings row.
func TestTryAutoDelegate_SameTask_StagedResumableRunHandled(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAbsorbScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectStagedResumable}
	router := newAbsorbTestRouter(database, sqlitestore.New(database).Conversations, stub)
	// Reproduce the real collision: see the comment on the sibling
	// InjectDeliveredLocal test above.
	bumpTaskViaRealUpsert(t, router, task, trigger, entityID, secondEventID)

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 StageOrDeliverAdditiveEvent attempt, got %d", len(stub.calls))
	}
	if stub.calls[0].runID != activeRunID {
		t.Errorf("attempt targeted run_id = %q, want %q", stub.calls[0].runID, activeRunID)
	}

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no pending_firings row (durably staged onto a resumable run counts as handled), got %d", len(rows))
	}

	var kind string
	if err := database.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, task.ID, secondEventID).Scan(&kind); err != nil {
		t.Fatalf("read task_events for injected bookkeeping: %v", err)
	}
	if kind != "injected" {
		t.Errorf("task_events.kind = %q, want %q", kind, "injected")
	}
}

// TestTryAutoDelegate_SameTask_DeliveredRemoteHandledWithoutRecording
// pins the cross-pod outcome: InjectDeliveredRemote means a live
// remote executor now owns recording task_events 'injected' (or
// compensating with a pending_firing if the run turns out dead by apply
// time) — the router must treat the firing as handled but must NOT record
// the task_event itself, or a slow/failed remote apply could leave a
// duplicate or premature bookkeeping row.
func TestTryAutoDelegate_SameTask_DeliveredRemoteHandledWithoutRecording(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAbsorbScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredRemote}
	router := newAbsorbTestRouter(database, sqlitestore.New(database).Conversations, stub)

	// tryAutoDelegate's `fired` return is a bare `return` (the named
	// return's zero value) on every injection branch — a pre-existing quirk
	// this test doesn't re-litigate; "handled" is verified through the side
	// effects below (no deferral, no local bookkeeping), matching the
	// sibling InjectDeliveredLocal/InjectStagedResumable tests' convention.
	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 StageOrDeliverAdditiveEvent attempt, got %d", len(stub.calls))
	}
	if stub.calls[0].runID != activeRunID {
		t.Errorf("attempt targeted run_id = %q, want %q", stub.calls[0].runID, activeRunID)
	}

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no pending_firings row (handed off, not deferred), got %d", len(rows))
	}

	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND event_id = ?`, task.ID, secondEventID).Scan(&count); err != nil {
		t.Fatalf("count task_events: %v", err)
	}
	if count != 0 {
		t.Errorf("router recorded task_events for a remote delivery — the owning executor must record it, got %d rows", count)
	}
}

// TestTryAutoDelegate_SiblingTask_FiresConcurrently: a firing whose own
// task has no live run fires immediately, even while a DIFFERENT task on the
// same entity is mid-run. Two tasks on one pull request are two different
// situations — that is what the task dedup key means — and each may have an
// agent working.
//
// It is also the negative case for absorption: the sibling's event must
// never be folded into an unrelated task's run (an approve→merge or a
// label-blueprint intent can't be swallowed by an in-flight fix run, and
// cross-team injection stays impossible).
func TestTryAutoDelegate_SiblingTask_FiresConcurrently(t *testing.T) {
	database := newTestDB(t)
	// Task A owns a live auto run on the entity.
	entityID, _, _, _ := setupAbsorbScenario(t, database)

	// Task B is a DIFFERENT task on the SAME entity, from a different event
	// type. Its firing lands while task A's run is live.
	registerAbsorbEventType(t, absorbOtherEventType)
	seedAbsorbEventCatalog(t, database, absorbOtherEventType)

	st := sqlitestore.New(database)
	otherEventID, err := st.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbOtherEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"cross":"task"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record cross-task event: %v", err)
	}

	createTestPrompt(t, database, domain.Prompt{ID: "p-cross", Name: "Cross", Body: "x", Source: "user"})
	taskB, _, err := testTaskStore(database).FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		entityID, absorbOtherEventType, "", otherEventID, 0.5)
	if err != nil {
		t.Fatalf("create cross task: %v", err)
	}

	trigB := domain.EventHandler{
		ID:                     "trigger-cross",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-cross",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              absorbOtherEventType,
		BreakerThreshold:       intPtr(4),
		MinAutonomySuitability: floatPtr(0),
		Enabled:                true,
	}
	createTriggerForTestRouting(t, database, trigB)
	if err := database.QueryRow(`SELECT blueprint_id FROM event_handlers WHERE id = ?`, trigB.ID).Scan(&trigB.BlueprintID); err != nil {
		t.Fatalf("resolve cross trigger blueprint id: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal, allowFire: true}
	router := newAbsorbTestRouter(database, st.Conversations, stub)

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, taskB, trigB, entityID, otherEventID, "")

	if len(stub.calls) != 0 {
		t.Errorf("expected no injection for a sibling-task firing, got %d StageOrDeliverAdditiveEvent call(s)", len(stub.calls))
	}
	if len(stub.delegated) != 1 || stub.delegated[0] != taskB.ID {
		t.Errorf("delegated = %v, want exactly [%s] — a busy sibling task must not block this one", stub.delegated, taskB.ID)
	}
	rows, err := st.PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected the sibling-task firing to fire outright, got %d deferred row(s)", len(rows))
	}
}

// TestTryAutoDelegate_SameTask_NotDeliveredFallsThroughToDeferral pins that
// a StageOrDeliverAdditiveEvent report of InjectNotDelivered (dropped
// outright, or staged onto a run that's since gone fully terminal — both
// decided inside internal/delegate now) always falls through to the normal
// deferral rather than being silently dropped.
func TestTryAutoDelegate_SameTask_NotDeliveredFallsThroughToDeferral(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, _ := setupAbsorbScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectNotDelivered}
	router := newAbsorbTestRouter(database, sqlitestore.New(database).Conversations, stub)

	fired := router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")
	if !fired {
		t.Error("expected tryAutoDelegate to report handled via the normal deferral (enqueueBusyFiring)")
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 StageOrDeliverAdditiveEvent attempt, got %d", len(stub.calls))
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the firing to fall through to the normal deferral, got %d pending_firings rows", len(rows))
	}
}

// TestTryAutoDelegate_SameTask_StageToNonResumableRun_NoOrphanedRow is the
// routing-level regression test for staging onto a non-resumable run.
// Unlike the sibling tests above, this one wires a REAL *delegate.Spawner as the Delegator
// (not injectingStubDelegator) so StageOrDeliverAdditiveEvent's actual DB
// side effects — the staged-injection message append AND its cleanup — are
// observable. setupAbsorbScenario's active run is status="running" with
// no live process registered against this fresh spawner instance, so
// getProc returns nil (no local delivery) and there's no conversation_signals wired
// (no remote path either) — StageOrDeliverAdditiveEvent falls to staging,
// appends the durable row, then its post-stage recheck reads the run back
// as status="running" (not "open", not completed+abort — resumableState
// false) and reports InjectNotDelivered. The caller must fall through to
// the normal pending_firings deferral, AND the staged row it appended along
// the way must be deleted rather than orphaned.
func TestTryAutoDelegate_SameTask_StageToNonResumableRun_NoOrphanedRow(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAbsorbScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	spawner := delegate.NewSpawner(database, sqlitestore.New(database), nil, nil, "m")
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), sqlitestore.New(database).Conversations, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		spawner, noopScorer{}, websocket.NewHub())

	fired := router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")
	if !fired {
		t.Error("expected tryAutoDelegate to report handled via the normal deferral (enqueueBusyFiring)")
	}

	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the firing to fall through to the normal deferral, got %d pending_firings rows", len(rows))
	}

	// Withdrawn-pending rows retire as window_state='inactive' (delivered
	// stays 0 — withdrawn means the note never happened), so "orphaned"
	// means an undelivered row still in the flushable set.
	var staged int
	if err := database.QueryRow(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND delivered = 0 AND window_state <> 'inactive' AND subtype = 'injection:system-note'`, activeRunID).Scan(&staged); err != nil {
		t.Fatalf("count staged injection messages: %v", err)
	}
	if staged != 0 {
		t.Errorf("messages holds %d orphaned staged-injection row(s) for run %q, want 0 (deferral landed but the staged row was never cleaned up)", staged, activeRunID)
	}
}

// TestTryAutoDelegate_SameTask_StampsAgentClaimOnInjectedTask pins that a
// successful injection is a commitment point exactly like the deferral path
// (post-Enqueue): the firing's own task gets claimed for the bot. In the
// same-task absorption model the run the event folds into belongs to this
// same task, so the claim is normally already the bot's — this asserts the
// stamp still lands (and doesn't regress) on the injected task.
func TestTryAutoDelegate_SameTask_StampsAgentClaimOnInjectedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, _ := setupAbsorbScenario(t, database)

	if _, err := database.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if task.ClaimedByAgentID != "" {
		t.Fatalf("precondition: task already claimed before the fire")
	}

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    absorbTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	st := sqlitestore.New(database)
	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal}
	// Agents wired (real store, a seeded agent to resolve) so agentID
	// resolves to something non-empty; teamAgents left nil so the
	// bot-disabled-team gate degrades to "proceed" and doesn't interfere.
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), st.Agents, nil, nil,
		testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings,
		st.Events, st.Orgs, st.Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected exactly 1 injection attempt, got %d", len(stub.calls))
	}

	got, err := testTaskStore(database).Get(context.Background(), runmode.LocalDefaultOrgID, task.ID)
	if err != nil || got == nil {
		t.Fatalf("re-read task: %v", err)
	}
	if got.ClaimedByAgentID != runmode.LocalDefaultAgentID {
		t.Errorf("ClaimedByAgentID = %q, want %q — a successful injection must commit the claim like the deferral path does",
			got.ClaimedByAgentID, runmode.LocalDefaultAgentID)
	}
}

// TestTryAutoDelegate_FrozenTask_BlocksOnlyItself is the regression this
// re-keying exists for. A conversation-level stop parks its conversation
// `open` and deliberately leaves the blueprint 'running' — a frozen step
// nothing will terminate until a human resumes it or dispositions the task.
// Under the former entity-keyed gate that one row held the gate shut for the
// whole entity, and because the drain is triggered by a run reaching a
// terminal, nothing was left to reopen it: every later event on that pull
// request queued behind a run that was never going to finish.
//
// Now the freeze is scoped to its own task. The frozen task still absorbs
// its own follow-up events (it has a live conversation to fold them into),
// and a sibling task fires immediately.
func TestTryAutoDelegate_FrozenTask_BlocksOnlyItself(t *testing.T) {
	database := newTestDB(t)
	entityID, taskA, trigA, activeRunID := setupAbsorbScenario(t, database)

	// Freeze task A exactly as a stop leaves it: conversation parked `open`,
	// blueprint still 'running'.
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = ?`, activeRunID); err != nil {
		t.Fatalf("park conversation: %v", err)
	}
	var bpStatus string
	if err := database.QueryRow(
		`SELECT status FROM blueprint_runs WHERE id = (SELECT blueprint_run_id FROM conversations WHERE id = ?)`, activeRunID,
	).Scan(&bpStatus); err != nil {
		t.Fatalf("read blueprint status: %v", err)
	}
	if bpStatus != "running" {
		t.Fatalf("fixture blueprint status = %q, want running (the frozen state under test)", bpStatus)
	}

	st := sqlitestore.New(database)

	// A sibling task on the same entity fires, unblocked.
	registerAbsorbEventType(t, absorbOtherEventType)
	seedAbsorbEventCatalog(t, database, absorbOtherEventType)
	siblingEventID, err := st.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: absorbOtherEventType, EntityID: &entityID,
		MetadataJSON: `{"sibling":true}`, CreatedAt: time.Now(), OrgID: runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record sibling event: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-sibling", Name: "Sibling", Body: "x", Source: "user"})
	taskB, _, err := testTaskStore(database).FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		entityID, absorbOtherEventType, "", siblingEventID, 0.5)
	if err != nil {
		t.Fatalf("create sibling task: %v", err)
	}
	trigB := domain.EventHandler{
		ID: "trigger-sibling", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p-sibling", TriggerType: domain.TriggerTypeEvent,
		EventType: absorbOtherEventType, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
	}
	createTriggerForTestRouting(t, database, trigB)
	if err := database.QueryRow(`SELECT blueprint_id FROM event_handlers WHERE id = ?`, trigB.ID).Scan(&trigB.BlueprintID); err != nil {
		t.Fatalf("resolve sibling blueprint id: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal, allowFire: true}
	router := newAbsorbTestRouter(database, st.Conversations, stub)
	router.tryAutoDelegate(runmode.LocalDefaultOrgID, taskB, trigB, entityID, siblingEventID, "")

	if len(stub.delegated) != 1 || stub.delegated[0] != taskB.ID {
		t.Errorf("sibling delegated = %v, want [%s] — a frozen task must not hold the entity", stub.delegated, taskB.ID)
	}
	if rows, _ := st.PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID); len(rows) != 0 {
		t.Errorf("sibling firing deferred behind a frozen task (%d row(s))", len(rows))
	}

	// The frozen task itself is still gated: its own follow-up folds into
	// the parked conversation rather than starting a second run on it.
	followupEventID, err := st.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: absorbTestEventType, EntityID: &entityID,
		MetadataJSON: `{"followup":true}`, CreatedAt: time.Now(), OrgID: runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record follow-up event: %v", err)
	}
	stub.delegated = nil
	router.tryAutoDelegate(runmode.LocalDefaultOrgID, taskA, trigA, entityID, followupEventID, "")

	if len(stub.delegated) != 0 {
		t.Errorf("frozen task fired a second run (%v); its own gate must stay closed", stub.delegated)
	}
	if len(stub.calls) != 1 || stub.calls[0].runID != activeRunID {
		t.Errorf("follow-up on the frozen task should fold into its parked conversation, got calls=%+v", stub.calls)
	}
}
