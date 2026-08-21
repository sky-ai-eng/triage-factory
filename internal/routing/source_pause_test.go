package routing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// pauseSource records an org admin's disable for kind, the way the PATCH route
// does.
func pauseSource(t *testing.T, database *sql.DB, kind string) {
	t.Helper()
	st := sqlitestore.New(database)
	if _, err := st.OrgEventSources.SetDisabled(
		context.Background(), runmode.LocalDefaultOrgID, kind, true, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("pause %s: %v", kind, err)
	}
}

// gatedRouter is reviewRouter with the event-source gate wired, which is the
// production shape. reviewRouter deliberately leaves it off so the ~30 existing
// routing tests keep routing.
func gatedRouter(t *testing.T, database *sql.DB) *Router {
	t.Helper()
	r := reviewRouter(database)
	r.SetEventSourceGate(sqlitestore.New(database).OrgEventSources)
	return r
}

// TestHandleEvent_PausedSource_RecordsButCreatesNoTask is the router's half of
// the event-source pause: the event is recorded — the append-only log stays an
// honest record of what happened while the source was off — and nothing
// ephemeral is derived from it. Resuming restores task creation on the next
// event, with no replay of what was dropped (forward-only, like every other
// tracking change).
func TestHandleEvent_PausedSource_RecordsButCreatesNoTask(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	pauseSource(t, database, "github")
	entityID := reviewEntity(t, database, "owner/repo#paused")
	emitCI(gatedRouter(t, database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("paused source: got %d active tasks, want 0", len(active))
	}
	var eventCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events WHERE entity_id = ?`, entityID).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("events recorded = %d, want 1 — a paused source's events are still logged", eventCount)
	}

	// Resume. A fresh router, because the gate caches per org and the
	// production invalidation (in process or over tf_ctl) is what a live one
	// would receive.
	st := sqlitestore.New(database)
	if _, err := st.OrgEventSources.SetDisabled(context.Background(), runmode.LocalDefaultOrgID, "github", false, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	emitCI(gatedRouter(t, database), entityID, "aidan")

	active, err = testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks after resume: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("after resuming: got %d active tasks, want 1", len(active))
	}
}

// TestHandleEvent_PausedSource_LeavesExistingTasksAlone pins the forward-only
// half: pausing stops NEW events from minting tasks and touches nothing that
// already exists. A task is durable work that may have an open PR and agent
// memory behind it, so a config change never destroys one.
func TestHandleEvent_PausedSource_LeavesExistingTasksAlone(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	entityID := reviewEntity(t, database, "owner/repo#pre-existing")
	emitCI(gatedRouter(t, database), entityID, "aidan")
	before, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(before) != 1 {
		t.Fatalf("precondition: tasks=%d err=%v, want 1 task", len(before), err)
	}

	pauseSource(t, database, "github")
	emitCI(gatedRouter(t, database), entityID, "aidan")

	after, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Errorf("after pausing: tasks=%d, want the one pre-existing task %s intact", len(after), before[0].ID)
	}
}

// TestHandleEvent_PausedSource_PublishesSourceDisabled pins that the drop is
// observable rather than silent: exactly one disposition sentinel, reporting
// source_disabled — distinct from frozen, which is a licence fault nobody in
// the org chose.
func TestHandleEvent_PausedSource_PublishesSourceDisabled(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)
	pauseSource(t, database, "github")

	router := gatedRouter(t, database)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	emitCI(router, reviewEntity(t, database, "owner/repo#disp"), "aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	if meta := decodeDisposition(t, got[0].MetadataJSON); meta.Disposition != events.DispositionSourceDisabled {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionSourceDisabled)
	}
}

// TestHandleEvent_PausedSource_IsPerSource is the negative control: pausing
// Jira must not stop GitHub routing.
func TestHandleEvent_PausedSource_IsPerSource(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)
	pauseSource(t, database, "jira")

	entityID := reviewEntity(t, database, "owner/repo#other-source")
	emitCI(gatedRouter(t, database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("pausing jira changed github routing: got %d active tasks, want 1", len(active))
	}
}

// erroringSourceStore fails every policy read, standing in for a transient
// database fault while the router is deciding whether a source is paused.
type erroringSourceStore struct{ dbpkg.OrgEventSourceStore }

func (erroringSourceStore) ListDisabledSystem(context.Context, string) ([]string, error) {
	return nil, errors.New("boom: org_event_sources read failed")
}

// TestHandleEvent_SourcePolicyReadFails_ErrorsRatherThanRoutes pins the
// fail-closed choice. The drain worker requeues an errored event, so a
// transient read costs a retry; answering "not paused" on a failed read would
// mint exactly the tasks the pause exists to prevent, and nothing would ever
// notice.
func TestHandleEvent_SourcePolicyReadFails_ErrorsRatherThanRoutes(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	router := reviewRouter(database)
	router.SetEventSourceGate(erroringSourceStore{})
	entityID := reviewEntity(t, database, "owner/repo#gate-fault")

	err := router.HandleEvent(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EventType: domain.EventGitHubPRCICheckFailed,
		EntityID:  &entityID,
		CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("HandleEvent returned nil on a failed policy read, want an error so the worker retries")
	}
	active, aerr := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if aerr != nil {
		t.Fatalf("list active tasks: %v", aerr)
	}
	if len(active) != 0 {
		t.Errorf("a failed policy read created %d tasks, want 0", len(active))
	}
}

// TestSourceGate_TTLExpiryRereads pins the cache's floor on correctness. The
// tf_ctl invalidation normally beats the TTL, but it is lossy by contract — a
// message published in a holderless gap reaches nobody — so the TTL is what
// bounds how long a DROPPED invalidation can keep events flowing for a paused
// source.
func TestSourceGate_TTLExpiryRereads(t *testing.T) {
	database := newTestDB(t)
	st := sqlitestore.New(database)
	now := time.Now()
	gate := newSourceGate(st.OrgEventSources)
	gate.now = func() time.Time { return now }

	ctx := context.Background()
	if paused, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, "github"); err != nil || paused {
		t.Fatalf("precondition: disabled=%v err=%v, want false", paused, err)
	}
	pauseSource(t, database, "github")

	// Still inside the TTL: the cached answer stands, which is the whole point
	// of caching a row that changes about once a quarter.
	if paused, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, "github"); err != nil || paused {
		t.Errorf("within TTL: disabled=%v err=%v, want the cached false", paused, err)
	}
	now = now.Add(sourceGateTTL + time.Second)
	if paused, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, "github"); err != nil || !paused {
		t.Errorf("past TTL: disabled=%v err=%v, want a fresh true", paused, err)
	}
}

// TestSourceGate_InvalidateDropsTheCachedAnswer pins the fast path the relay
// drives: an admin's write reaches the brain and the next event re-reads,
// rather than waiting out the TTL.
func TestSourceGate_InvalidateDropsTheCachedAnswer(t *testing.T) {
	database := newTestDB(t)
	gate := newSourceGate(sqlitestore.New(database).OrgEventSources)
	ctx := context.Background()

	if _, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, "github"); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	pauseSource(t, database, "github")
	gate.invalidate(runmode.LocalDefaultOrgID)

	if paused, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, "github"); err != nil || !paused {
		t.Errorf("after invalidate: disabled=%v err=%v, want true without waiting out the TTL", paused, err)
	}
}
