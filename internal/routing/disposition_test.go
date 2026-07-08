package routing

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

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
	if meta.TriggersFired < 1 {
		t.Errorf("triggers_fired = %d, want >= 1", meta.TriggersFired)
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
