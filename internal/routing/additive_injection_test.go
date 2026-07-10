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

// additiveTestEventType is a synthetic event type these tests register with
// Additive=true, mirroring fakeEventType's out-of-core-source pattern
// (source_registry_test.go) rather than mutating a real registered type
// (e.g. ci_check_failed) for the duration of a test. Registered per-test
// (not via init()) and cleaned up with t.Cleanup(events.Reset(...)), so it
// never leaks into sibling tests.
const additiveTestEventType = "fake:additive:mentioned"

// registerAdditiveTestEventType registers additiveTestEventType as
// Additive=true (match-all predicate) and schedules its removal from the
// registry at test end.
func registerAdditiveTestEventType(t *testing.T) {
	t.Helper()
	events.Register(events.EventSchema{
		EventType: additiveTestEventType,
		Ownership: events.OwnershipOwned,
		Additive:  true,
		Match: func(predJSON, metaJSON string) (bool, error) {
			return true, nil
		},
	})
	t.Cleanup(func() { events.Reset(additiveTestEventType) })
}

// seedAdditiveTestEventCatalog inserts the events_catalog row
// additiveTestEventType needs to satisfy the event_handlers.event_type /
// events.event_type / tasks.event_type FKs — mirrors
// seedFakeEventCatalog in source_registry_test.go.
func seedAdditiveTestEventCatalog(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`
		INSERT OR IGNORE INTO events_catalog (id, source, category, label, description)
		VALUES (?, 'fake', 'additive', 'Fake Additive Mentioned', 'additive-injection test fixture')
	`, additiveTestEventType); err != nil {
		t.Fatalf("seed events_catalog for %s: %v", additiveTestEventType, err)
	}
}

// injectingStubDelegator is a Delegator fake for the additive-injection
// gate tests: it never fires a new run (the busy-entity branch under test
// never calls Delegate) and records every StageOrDeliverAdditiveEvent call
// (including the firing ref) so tests can assert what got routed, where.
// outcome controls the return value — tryAdditiveInjection's dispatch logic
// is what these tests exercise; StageOrDeliverAdditiveEvent's own internal
// decision-making (local/remote/staged/resumable) is covered directly in
// internal/delegate.
type injectingStubDelegator struct {
	outcome delegate.InjectOutcome
	calls   []injectCall
}

type injectCall struct {
	orgID, runID, producer, body string
	firing                       delegate.AdditiveFiringRef
}

func (s *injectingStubDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	return "", fmt.Errorf("unexpected Delegate call: additive-injection tests only exercise the busy-entity branch")
}

func (s *injectingStubDelegator) Cancel(orgID, runID, userID string) error { return nil }

func (s *injectingStubDelegator) StageOrDeliverAdditiveEvent(ctx context.Context, orgID, runID, producer, body string, firing delegate.AdditiveFiringRef) delegate.InjectOutcome {
	s.calls = append(s.calls, injectCall{orgID, runID, producer, body, firing})
	return s.outcome
}

// activeAutoRunIDOverrideStore wraps a real AgentRunStore, overriding
// ActiveAutoRunIDForEntitySystem to always report no resolvable run — models
// the TOCTOU race where HasActiveAutoRunForEntitySystem (read moments
// earlier by the gate) saw an active run, but by the time the injection
// branch tries to resolve its ID, it's gone (raced to terminal).
type activeAutoRunIDOverrideStore struct {
	db.AgentRunStore
}

func (activeAutoRunIDOverrideStore) ActiveAutoRunIDForEntitySystem(ctx context.Context, orgID, entityID string) (string, error) {
	return "", nil
}

// setupAdditiveScenario seeds entity + prompt + a trigger on
// additiveTestEventType + an initial event, then delegates it into an
// active run (mirroring a real in-flight auto run) via stubDelegateRun.
// Returns the entity, the task the trigger matches, the trigger (with its
// real resolved blueprint id), and the active run's ID.
func setupAdditiveScenario(t *testing.T, database *sql.DB) (entityID string, task *domain.Task, trigger domain.EventHandler, activeRunID string) {
	t.Helper()
	registerAdditiveTestEventType(t)
	seedAdditiveTestEventCatalog(t, database)

	st := sqlitestore.New(database)
	entity, _, err := st.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "fake", "fake/thing#"+uuid.New().String()[:8], "thing",
		"Additive Thing", "https://example.com/additive")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	entityID = entity.ID

	createTestPrompt(t, database, domain.Prompt{ID: "p-additive", Name: "Additive", Body: "x", Source: "user"})

	firstEventID, err := st.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"first":true}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record first event: %v", err)
	}

	gotTask, _, err := testTaskStore(database).FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		entityID, additiveTestEventType, "", firstEventID, 0.5)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task = gotTask

	trig := domain.EventHandler{
		ID:                     "trigger-additive",
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "p-additive",
		TriggerType:            domain.TriggerTypeEvent,
		EventType:              additiveTestEventType,
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
	if err := database.QueryRow(`SELECT id FROM runs WHERE blueprint_run_id = ?`, blueprintRunID).Scan(&activeRunID); err != nil {
		t.Fatalf("resolve seeded run id: %v", err)
	}
	return
}

// newAdditiveTestRouter builds a Router wired for the additive-injection
// gate tests, sharing the same store construction every test below needs.
func newAdditiveTestRouter(database *sql.DB, agentRuns db.AgentRunStore, stub *injectingStubDelegator) *Router {
	st := sqlitestore.New(database)
	return NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), agentRuns, st.Entities, st.PendingFirings,
		st.Events, st.Orgs, st.Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())
}

// TestTryAutoDelegate_AdditiveEvent_InjectsIntoActiveRun is the ticket's
// primary acceptance: an Additive-flagged trigger firing against an entity
// with an active auto run injects into that run (StageOrDeliverAdditiveEvent
// reporting InjectDeliveredLocal) instead of deferring — no pending_firings
// row lands, and the task_events row records the injection.
func TestTryAutoDelegate_AdditiveEvent_InjectsIntoActiveRun(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAdditiveScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal}
	router := newAdditiveTestRouter(database, sqlitestore.New(database).AgentRuns, stub)

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected exactly 1 StageOrDeliverAdditiveEvent call, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	if got.runID != activeRunID {
		t.Errorf("injected run_id = %q, want the active run %q", got.runID, activeRunID)
	}
	if got.producer != additiveTestEventType {
		t.Errorf("injected producer = %q, want %q", got.producer, additiveTestEventType)
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

// TestTryAutoDelegate_AdditiveEvent_StagedResumableRunHandled is the success
// counterpart above for the "durably staged onto a resumable run" outcome —
// StageOrDeliverAdditiveEvent reporting InjectStagedResumable must be
// treated as handled exactly like InjectDeliveredLocal: task_events
// recorded, no pending_firings row.
func TestTryAutoDelegate_AdditiveEvent_StagedResumableRunHandled(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAdditiveScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectStagedResumable}
	router := newAdditiveTestRouter(database, sqlitestore.New(database).AgentRuns, stub)

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

// TestTryAutoDelegate_AdditiveEvent_DeliveredRemoteHandledWithoutRecording
// pins TFAC-585's cross-pod outcome: InjectDeliveredRemote means a live
// remote executor now owns recording task_events 'injected' (or
// compensating with a pending_firing if the run turns out dead by apply
// time) — the router must treat the firing as handled but must NOT record
// the task_event itself, or a slow/failed remote apply could leave a
// duplicate or premature bookkeeping row.
func TestTryAutoDelegate_AdditiveEvent_DeliveredRemoteHandledWithoutRecording(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, activeRunID := setupAdditiveScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredRemote}
	router := newAdditiveTestRouter(database, sqlitestore.New(database).AgentRuns, stub)

	// tryAutoDelegate's `fired` return is a bare `return` (the named
	// return's zero value) on every additive-injection branch — a
	// pre-existing quirk this test doesn't re-litigate; "handled" is
	// verified through the side effects below (no deferral, no local
	// bookkeeping), matching the sibling InjectDeliveredLocal/
	// InjectStagedResumable tests' convention.
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

// TestTryAutoDelegate_NonAdditiveEvent_StillDefers pins that the injection
// branch is additive-only: a busy entity with a non-additive trigger keeps
// today's behavior exactly — deferred onto pending_firings, no injection
// attempted.
func TestTryAutoDelegate_NonAdditiveEvent_StillDefers(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, _ := setupAdditiveScenario(t, database)
	// Overwrite the registration without Additive so this test's trigger
	// fires a non-additive type — same event type, different schema, still
	// cleaned up by setupAdditiveScenario's registerAdditiveTestEventType
	// t.Cleanup (Reset is idempotent on a since-overwritten entry).
	events.Reset(additiveTestEventType)
	events.Register(events.EventSchema{
		EventType: additiveTestEventType,
		Ownership: events.OwnershipOwned,
		Additive:  false,
		Match:     func(predJSON, metaJSON string) (bool, error) { return true, nil },
	})

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal}
	router := newAdditiveTestRouter(database, sqlitestore.New(database).AgentRuns, stub)

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 0 {
		t.Errorf("expected no StageOrDeliverAdditiveEvent call for a non-additive type, got %d", len(stub.calls))
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 deferred pending_firings row, got %d", len(rows))
	}
}

// TestTryAutoDelegate_AdditiveEvent_RunIDRaceFallsThroughToDeferral covers
// the load-bearing race the ticket calls out: the busy gate saw an active
// run (HasActiveAutoRunForEntitySystem=true), but by the time the injection
// branch tries to resolve its ID, ActiveAutoRunIDForEntitySystem comes back
// empty (the run ended in between). The firing must never be silently
// dropped — it falls through to the normal deferral.
func TestTryAutoDelegate_AdditiveEvent_RunIDRaceFallsThroughToDeferral(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, _ := setupAdditiveScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectDeliveredLocal}
	racedStore := activeAutoRunIDOverrideStore{AgentRunStore: sqlitestore.New(database).AgentRuns}
	router := newAdditiveTestRouter(database, racedStore, stub)

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 0 {
		t.Errorf("expected no StageOrDeliverAdditiveEvent call when the run ID can't be resolved, got %d", len(stub.calls))
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the firing to fall through to the normal deferral, got %d pending_firings rows", len(rows))
	}
}

// TestTryAutoDelegate_AdditiveEvent_NotDeliveredFallsThroughToDeferral pins
// that a StageOrDeliverAdditiveEvent report of InjectNotDelivered (dropped
// outright, or staged onto a run that's since gone fully terminal — both
// decided inside internal/delegate now) always falls through to the normal
// deferral rather than being silently dropped.
func TestTryAutoDelegate_AdditiveEvent_NotDeliveredFallsThroughToDeferral(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, _ := setupAdditiveScenario(t, database)

	secondEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    additiveTestEventType,
		EntityID:     &entityID,
		MetadataJSON: `{"mention":"second"}`,
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})
	if err != nil {
		t.Fatalf("record second event: %v", err)
	}

	stub := &injectingStubDelegator{outcome: delegate.InjectNotDelivered}
	router := newAdditiveTestRouter(database, sqlitestore.New(database).AgentRuns, stub)

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

// TestTryAutoDelegate_AdditiveEvent_StampsAgentClaimOnInjectedTask pins that
// a successful injection is a commitment point exactly like the deferral
// path (post-Enqueue): the firing's OWN task gets claimed for the bot, even
// though — per HasActiveAutoRunForEntitySystem's doc comment, the busy
// gate is entity-wide, not task-scoped — the run the event got folded into
// may belong to a DIFFERENT task on the same entity. Without this, an
// injected task could sit unclaimed in the board's queue lanes despite its
// event already having been forwarded into a live agent conversation.
func TestTryAutoDelegate_AdditiveEvent_StampsAgentClaimOnInjectedTask(t *testing.T) {
	database := newTestDB(t)
	entityID, task, trigger, _ := setupAdditiveScenario(t, database)

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
		EventType:    additiveTestEventType,
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
		testTaskStore(database), st.AgentRuns, st.Entities, st.PendingFirings,
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
