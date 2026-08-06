package routing

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// erroringHandlerStore wraps a real EventHandlerStore and forces
// GetEnabledForEventSystem to fail, simulating a transient query error
// distinct from "queried fine, zero handlers matched."
type erroringHandlerStore struct {
	db.EventHandlerStore
}

func (erroringHandlerStore) GetEnabledForEventSystem(ctx context.Context, orgID, eventType string) ([]domain.EventHandler, error) {
	return nil, errors.New("boom: event_handlers query failed")
}

// erroringTaskStore wraps a real TaskStore and forces FindOrCreateAtSystem
// to fail, simulating a task-upsert storage failure distinct from the
// became_atomic dedup suppression (a legitimate "no task").
type erroringTaskStore struct {
	db.TaskStore
}

func (erroringTaskStore) FindOrCreateAtSystem(ctx context.Context, orgID, teamID, entityID, eventType, dedupKey, primaryEventID string, defaultPriority float64, createdAt time.Time) (*domain.Task, bool, error) {
	return nil, false, errors.New("boom: task upsert failed")
}

// fakeDispositionPublisher captures every published event so tests can
// assert the router's per-event routing-disposition sentinel (TFAC-593)
// without a real eventbus.Bus. Mirrors internal/delegate's
// fakeEventPublisher (event_publisher_test.go), which pins the same
// EventPublisher shape for the spawner's run-lifecycle mirror.
type fakeDispositionPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (f *fakeDispositionPublisher) Publish(evt domain.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evt)
}

func (f *fakeDispositionPublisher) eventsCopy() []domain.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Event, len(f.events))
	copy(out, f.events)
	return out
}

func decodeDisposition(t *testing.T, raw string) events.SystemRoutingDispositionMetadata {
	t.Helper()
	var meta events.SystemRoutingDispositionMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("decode SystemRoutingDispositionMetadata: %v", err)
	}
	return meta
}

// TestHandleEvent_EntitlementGated_PublishesFrozen pins the entitlement-gate
// branch: a gated-off source still publishes exactly one disposition
// sentinel, and it reports "frozen".
func TestHandleEvent_EntitlementGated_PublishesFrozen(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	entitlements.GateEventSource("github", testGateFeature)
	t.Cleanup(entitlements.Reset)

	router := reviewRouter(database)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	entityID := reviewEntity(t, database, "owner/repo#frozen")
	emitCI(router, entityID, "aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	if got[0].EventType != domain.EventSystemRoutingDisposition {
		t.Errorf("event_type = %q, want %q", got[0].EventType, domain.EventSystemRoutingDisposition)
	}
	if got[0].OrgID != runmode.LocalDefaultOrgID {
		t.Errorf("org_id = %q, want %q", got[0].OrgID, runmode.LocalDefaultOrgID)
	}
	meta := decodeDisposition(t, got[0].MetadataJSON)
	if meta.Disposition != events.DispositionFrozen {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionFrozen)
	}
	if meta.EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("event_type in metadata = %q, want %q", meta.EventType, domain.EventGitHubPRCICheckFailed)
	}
}

// TestHandleEvent_NoHandlerMatched_PublishesTasklessNoHandler pins the
// no-match branch: a registered event type with zero event_handlers rows
// published "taskless_no_handler".
func TestHandleEvent_NoHandlerMatched_PublishesTasklessNoHandler(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	// No event_handlers seeded at all (shipped triggers ship disabled and
	// this fixture doesn't call Seed), so nothing can match.

	router := reviewRouter(database)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	entityID := reviewEntity(t, database, "owner/repo#nohandler")
	emitCI(router, entityID, "aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	meta := decodeDisposition(t, got[0].MetadataJSON)
	if meta.Disposition != events.DispositionTasklessNoHandler {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionTasklessNoHandler)
	}
	if meta.EntityID != entityID {
		t.Errorf("entity_id = %q, want %q", meta.EntityID, entityID)
	}
}

// TestHandleEvent_NoOwnerResolved_PublishesTasklessNoOwner pins the
// resolveTeamRouting ok=false branch: a matched (non-watch) rule with no
// resolvable owner — the stranger-PR scenario from
// TestAuthorCentric_StrangerPR_NoTask — published "taskless_no_owner".
func TestHandleEvent_NoOwnerResolved_PublishesTasklessNoOwner(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan") // the local user
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)        // not applies_to_unowned

	router := reviewRouter(database)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	entityID := reviewEntity(t, database, "owner/repo#stranger")
	emitCI(router, entityID, "stranger") // not a TF user — no owner resolves

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	meta := decodeDisposition(t, got[0].MetadataJSON)
	if meta.Disposition != events.DispositionTasklessNoOwner {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionTasklessNoOwner)
	}
}

// TestHandleEvent_MatchingHandler_PublishesTaskCreatedThenBumped pins the
// two "real work happened" outcomes together, in sequence on the same
// entity: the first event creates a task and fires its trigger
// (task_created, TriggersFired >= 1, OwnerTeamID/TaskID set); the second
// event of the same type on the same entity finds the existing task
// (task_bumped, same TaskID).
func TestHandleEvent_MatchingHandler_PublishesTaskCreatedThenBumped(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedImmediateTrigger(t, database, runmode.LocalDefaultTeamID, domain.EventGitHubPRCICheckFailed, "ci-disp")
	enableTeamAutoDelegate(t, database, runmode.LocalDefaultTeamID)

	stub := &stubDelegator{db: database}
	router := firingRouter(database, stub)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	entityID := reviewEntity(t, database, "owner/repo#createdthenbumped")
	emitCI(router, entityID, "aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events after first event, want 1", len(got))
	}
	meta := decodeDisposition(t, got[0].MetadataJSON)
	if meta.Disposition != events.DispositionTaskCreated {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionTaskCreated)
	}
	if meta.TriggersFired != 1 {
		t.Errorf("triggers_fired = %d, want 1 (the single seeded trigger commits with no contention)", meta.TriggersFired)
	}
	if meta.OwnerTeamID != runmode.LocalDefaultTeamID {
		t.Errorf("owner_team_id = %q, want %q", meta.OwnerTeamID, runmode.LocalDefaultTeamID)
	}
	if meta.TaskID == "" {
		t.Error("task_id is empty, want the created task's id")
	}
	if meta.EntityID != entityID {
		t.Errorf("entity_id = %q, want %q", meta.EntityID, entityID)
	}
	firstTaskID := meta.TaskID

	// A second ci_check_failed on the same entity finds the existing task
	// (same dedup unit) instead of creating a new one.
	emitCI(router, entityID, "aidan")

	got = pub.eventsCopy()
	if len(got) != 2 {
		t.Fatalf("published %d events after second event, want 2", len(got))
	}
	meta = decodeDisposition(t, got[1].MetadataJSON)
	if meta.Disposition != events.DispositionTaskBumped {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionTaskBumped)
	}
	if meta.TaskID != firstTaskID {
		t.Errorf("task_id = %q, want the same task %q as the first event", meta.TaskID, firstTaskID)
	}
}

// TestHandleEvent_NilPublisher_NoPanicNoBehaviorChange pins the nil-guard:
// a router with no publisher wired (the default — most existing tests
// never call SetEventPublisher) routes exactly as before, no panic.
func TestHandleEvent_NilPublisher_NoPanicNoBehaviorChange(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#nilpub")
	emitCI(reviewRouter(database), entityID, "aidan") // no SetEventPublisher call

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 task, got %d — nil publisher must not change routing behavior", len(active))
	}
	if teamIDValue(&active[0]) != runmode.LocalDefaultTeamID {
		t.Errorf("owner = %q, want %q", teamIDValue(&active[0]), runmode.LocalDefaultTeamID)
	}
}

// TestHandleEvent_HandlerQueryError_PublishesError pins the fix for a
// review finding: a event_handlers query failure must not be reported as
// "taskless_no_handler" (which reads as "legitimately not configured") —
// it gets its own disposition, and must not silently create a task either.
func TestHandleEvent_HandlerQueryError_PublishesError(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")

	st := sqlitestore.New(database)
	router := NewRouter(
		testPromptStore(database), testBlueprintStore(database), erroringHandlerStore{st.EventHandlers}, nil, nil, st.Users,
		testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, st.TeamGitHubGroups, nil, noopScorer{}, websocket.NewHub(),
	)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	entityID := reviewEntity(t, database, "owner/repo#handlererr")
	emitCI(router, entityID, "aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	meta := decodeDisposition(t, got[0].MetadataJSON)
	if meta.Disposition != events.DispositionError {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionError)
	}

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("expected no task when the handler query failed, got %d", len(active))
	}
}

// TestHandleEvent_TaskUpsertError_PublishesError pins the fix for a review
// finding: a task-upsert storage failure must not be folded into
// taskless_unroutable alongside the legitimate became_atomic dedup
// suppression — it gets its own disposition.
func TestHandleEvent_TaskUpsertError_PublishesError(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	st := sqlitestore.New(database)
	router := NewRouter(
		testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, st.Users,
		erroringTaskStore{st.Tasks}, st.Conversations, st.Entities, st.PendingFirings, st.Events,
		st.Orgs, st.Teams, nil, nil, st.TeamGitHubGroups, nil, noopScorer{}, websocket.NewHub(),
	)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	entityID := reviewEntity(t, database, "owner/repo#upserterr")
	emitCI(router, entityID, "aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	meta := decodeDisposition(t, got[0].MetadataJSON)
	if meta.Disposition != events.DispositionError {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionError)
	}
	if meta.TaskID != "" {
		t.Errorf("task_id = %q, want empty on a store failure", meta.TaskID)
	}
}

// TestHandleEvent_BecameAtomicSuppression_PublishesTasklessNotError pins
// the other half of the same fix: the became_atomic dedup suppression is a
// legitimate "no task" outcome, not a failure — it must NOT surface as
// DispositionError just because upsertTaskForEvent's ok/err split now
// exists.
func TestHandleEvent_BecameAtomicSuppression_PublishesTasklessNotError(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	stores := sqlitestore.New(database)

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-dispsuppress", "issue", "Suppress", "https://example.com/suppress")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	seedEventID, err := stores.Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType: domain.EventJiraIssueAssigned, EntityID: &entity.ID, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record seed event: %v", err)
	}
	if _, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, "", seedEventID, 0.5); err != nil {
		t.Fatalf("seed pre-existing active task: %v", err)
	}
	seedSystemRule(t, database, runmode.LocalDefaultTeamID, domain.EventJiraIssueBecameAtomic)

	router := NewRouter(
		testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil,
		testTaskStore(database), stores.Conversations, stores.Entities, stores.PendingFirings, stores.Events,
		stores.Orgs, stores.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub(),
	)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	meta := events.JiraIssueBecameAtomicMetadata{IssueKey: "SKY-dispsuppress"}
	metaJSON, _ := json.Marshal(meta)
	router.HandleEvent(context.Background(), domain.Event{
		EventType: domain.EventJiraIssueBecameAtomic, EntityID: &entity.ID,
		MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
	})

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	gotMeta := decodeDisposition(t, got[0].MetadataJSON)
	if gotMeta.Disposition != events.DispositionTasklessUnroutable {
		t.Errorf("disposition = %q, want %q (legitimate suppression, not an error)", gotMeta.Disposition, events.DispositionTasklessUnroutable)
	}
}

// TestHandleEvent_MultipleTeams_TriggersFiredCountsOnlyCommitted pins the
// fix for a review finding: fireMatchedTriggers must count triggers that
// actually committed the bot (fired or enqueued), not every trigger merely
// attempted. Two teams both match jira:issue:available with an immediate
// trigger; the first team to run wins the exclusive claim and the second
// hits "already claimed by the bot for another team" inside
// tryAutoDelegate — a no-op that must NOT inflate TriggersFired.
func TestHandleEvent_MultipleTeams_TriggersFiredCountsOnlyCommitted(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	stores := sqlitestore.New(database)

	teamA := runmode.LocalDefaultTeamID
	teamB := "00000000-0000-0000-0000-0000000000b1"
	if _, err := database.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'team-b-dispfired', 'Team B Disp')`, teamB, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed team B: %v", err)
	}
	if _, err := database.Exec(`INSERT OR IGNORE INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, teamA); err != nil {
		t.Fatalf("seed team A settings: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO team_settings (team_id, auto_delegate_enabled) VALUES (?, 1)`, teamB); err != nil {
		t.Fatalf("seed team B settings: %v", err)
	}
	if _, err := database.Exec(`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`, runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(context.Background(), runmode.LocalDefaultOrgID, teamA, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team A: %v", err)
	}
	if err := stores.TeamAgents.AddForTeam(context.Background(), runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultAgentID); err != nil {
		t.Fatalf("add agent to team B: %v", err)
	}

	entity, _, err := stores.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-dispfired", "issue", "Disp fired", "https://example.com/dispfired")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	createTestPrompt(t, database, domain.Prompt{ID: "p-dispfired", Name: "Disp fired", Body: "x", Source: "user"})
	insertPromptForTeam(t, database, "p-dispfired-b", teamB)

	createTriggerForTestRouting(t, database, domain.EventHandler{
		ID: "trigger-A-dispfired", Kind: domain.EventHandlerKindTrigger,
		BlueprintID: "p-dispfired", TriggerType: domain.TriggerTypeEvent,
		EventType: domain.EventJiraIssueAvailable, BreakerThreshold: intPtr(4),
		MinAutonomySuitability: floatPtr(0), Enabled: true,
	})
	bpDispfiredB := insertBlueprintForTeam(t, database, "bp-dispfired-b", "p-dispfired-b", teamB)
	if _, err := database.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, creator_user_id, kind, event_type,
			 scope_predicate_json, enabled, source,
			 blueprint_id, breaker_threshold, min_autonomy_suitability,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, 'trigger', ?, NULL, 1, 'user', ?, 4, 0, datetime('now'), datetime('now'))
	`, "trigger-B-dispfired", runmode.LocalDefaultOrgID, teamB, runmode.LocalDefaultUserID,
		domain.EventJiraIssueAvailable, bpDispfiredB); err != nil {
		t.Fatalf("seed team B trigger: %v", err)
	}

	meta := events.JiraIssueAvailableMetadata{IssueKey: "SKY-dispfired", Project: "SKY", Status: "To Do"}
	metaJSON, _ := json.Marshal(meta)

	stub := &stubDelegator{db: database}
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), stores.Agents, stores.TeamAgents, nil, testTaskStore(database), stores.Conversations, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, nil, nil, nil, stub, noopScorer{}, websocket.NewHub())
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	router.HandleEvent(context.Background(), domain.Event{
		EventType: domain.EventJiraIssueAvailable, EntityID: &entity.ID,
		MetadataJSON: string(metaJSON), OrgID: runmode.LocalDefaultOrgID,
	})

	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 bot run (exclusive claim), got %d", stub.calls)
	}

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	gotMeta := decodeDisposition(t, got[0].MetadataJSON)
	if gotMeta.Disposition != events.DispositionTaskCreated {
		t.Errorf("disposition = %q, want %q", gotMeta.Disposition, events.DispositionTaskCreated)
	}
	if gotMeta.TriggersFired != 1 {
		t.Errorf("triggers_fired = %d, want 1 — both teams' triggers were ATTEMPTED, but only the winning team's actually committed the bot", gotMeta.TriggersFired)
	}
}
