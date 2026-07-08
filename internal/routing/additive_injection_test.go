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

// injectingStubDelegator is a Delegator fake for the additive-injection gate
// tests: it never fires a new run (the busy-entity branch under test never
// calls Delegate) and records every StageOrDeliverInjectionResult call so
// tests can assert what got injected, where. delivered/staged control the
// return value, mirroring *delegate.Spawner's three real outcomes:
// delivered=true (steered live), delivered=false+staged=true (durably
// queued), or both false (dropped — no live process AND the durable append
// itself failed).
type injectingStubDelegator struct {
	delivered bool
	staged    bool
	calls     []injectCall
}

type injectCall struct {
	orgID, runID, producer, body string
}

func (s *injectingStubDelegator) Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error) {
	return "", fmt.Errorf("unexpected Delegate call: additive-injection tests only exercise the busy-entity branch")
}

func (s *injectingStubDelegator) Cancel(orgID, runID, userID string) error { return nil }

func (s *injectingStubDelegator) StageOrDeliverInjectionResult(orgID, runID, producer, body string) (delivered, staged bool) {
	s.calls = append(s.calls, injectCall{orgID, runID, producer, body})
	return s.delivered, s.staged
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

// terminalGetOverrideStore wraps a real AgentRunStore, overriding GetSystem
// to report the run as fully terminal (completed+finish, not
// parked/resumable) regardless of its real row state — models the race
// where the run goes terminal between the ID resolution and
// StageOrDeliverInjectionResult's live-process check.
type terminalGetOverrideStore struct {
	db.AgentRunStore
}

func (terminalGetOverrideStore) GetSystem(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	return &domain.AgentRun{ID: runID, Status: "completed", Outcome: string(domain.RunOutcomeFinish)}, nil
}

// resumableGetOverrideStore wraps a real AgentRunStore, overriding GetSystem
// to report the run as parked `open` (resumable) regardless of its real row
// state — used to isolate the staged/dropped distinction from the run's
// terminal-ness: the seeded scenario run never actually leaves `running`
// (runIsResumable("running", "") is false), so tests that want to assert
// behavior specifically for a *resumable* run need this override.
type resumableGetOverrideStore struct {
	db.AgentRunStore
}

func (resumableGetOverrideStore) GetSystem(ctx context.Context, orgID, runID string) (*domain.AgentRun, error) {
	return &domain.AgentRun{ID: runID, Status: "open"}, nil
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

// TestTryAutoDelegate_AdditiveEvent_InjectsIntoActiveRun is the ticket's
// primary acceptance: an Additive-flagged trigger firing against an entity
// with an active auto run injects into that run via the staged-injection
// seam instead of deferring — no pending_firings row lands, and the
// injection carries the triggering event's type + metadata.
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

	stub := &injectingStubDelegator{delivered: true}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected exactly 1 StageOrDeliverInjectionResult call, got %d", len(stub.calls))
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

	stub := &injectingStubDelegator{delivered: true}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), sqlitestore.New(database).AgentRuns, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 0 {
		t.Errorf("expected no StageOrDeliverInjectionResult call for a non-additive type, got %d", len(stub.calls))
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

	stub := &injectingStubDelegator{delivered: true}
	racedStore := activeAutoRunIDOverrideStore{AgentRunStore: sqlitestore.New(database).AgentRuns}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), racedStore, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 0 {
		t.Errorf("expected no StageOrDeliverInjectionResult call when the run ID can't be resolved, got %d", len(stub.calls))
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the firing to fall through to the normal deferral, got %d pending_firings rows", len(rows))
	}
}

// TestTryAutoDelegate_AdditiveEvent_StagedTerminalRunFallsThroughToDeferral
// covers the load-bearing race: the injection was durably staged
// (delivered=false, staged=true) because the run has no warm process, and
// the run has ALSO already gone fully terminal by the time we re-check — a
// run that never resumes would never flush the staged row. The firing must
// fall through to the normal deferral rather than being silently dropped.
func TestTryAutoDelegate_AdditiveEvent_StagedTerminalRunFallsThroughToDeferral(t *testing.T) {
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

	stub := &injectingStubDelegator{delivered: false, staged: true} // durably staged, not delivered live
	racedStore := terminalGetOverrideStore{AgentRunStore: sqlitestore.New(database).AgentRuns}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), racedStore, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 StageOrDeliverInjectionResult attempt (staged, not delivered), got %d", len(stub.calls))
	}
	if stub.calls[0].runID != activeRunID {
		t.Errorf("staged attempt targeted run_id = %q, want %q", stub.calls[0].runID, activeRunID)
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the firing to fall through to the normal deferral, got %d pending_firings rows", len(rows))
	}
}

// TestTryAutoDelegate_AdditiveEvent_StagedResumableRunHandled is the success
// counterpart to the terminal-run race above: the injection was durably
// staged (delivered=false, staged=true) and the run is still resumable
// (parked `open`) — the firing must be treated as handled, exactly like the
// delivered-live case, with no pending_firings row.
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

	stub := &injectingStubDelegator{delivered: false, staged: true} // durably staged, not delivered live
	resumableStore := resumableGetOverrideStore{AgentRunStore: sqlitestore.New(database).AgentRuns}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), resumableStore, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 StageOrDeliverInjectionResult attempt, got %d", len(stub.calls))
	}
	if stub.calls[0].runID != activeRunID {
		t.Errorf("staged attempt targeted run_id = %q, want %q", stub.calls[0].runID, activeRunID)
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

// TestTryAutoDelegate_AdditiveEvent_DroppedStagingFallsThroughToDeferral
// pins the fix for the real gap a bare bool return can't express: when
// StageOrDeliverInjectionResult reports the injection was dropped outright
// (delivered=false, staged=false — no live process AND the durable append
// itself failed), there is no durable row anywhere to fall back on. This
// must fall through to the normal deferral UNCONDITIONALLY — even when the
// run's own status looks resumable, which a bare-bool caller could
// mistake for "already handled" and silently lose the firing.
func TestTryAutoDelegate_AdditiveEvent_DroppedStagingFallsThroughToDeferral(t *testing.T) {
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

	stub := &injectingStubDelegator{delivered: false, staged: false} // dropped outright
	// Deliberately make the run look resumable — proves the fall-through
	// doesn't depend on (and isn't fooled by) the run's status.
	resumableStore := resumableGetOverrideStore{AgentRunStore: sqlitestore.New(database).AgentRuns}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), resumableStore, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings,
		sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil,
		stub, noopScorer{}, websocket.NewHub())

	router.tryAutoDelegate(runmode.LocalDefaultOrgID, task, trigger, entityID, secondEventID, "")

	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 StageOrDeliverInjectionResult attempt, got %d", len(stub.calls))
	}
	rows, err := sqlitestore.New(database).PendingFirings.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list pending firings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the firing to fall through to the normal deferral on a genuine drop, got %d pending_firings rows", len(rows))
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
	stub := &injectingStubDelegator{delivered: true}
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
