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
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// These drive Jira because Jira is a source an org can turn off. GitHub is not
// (eventsource.Disableable) — TestHandleEvent_RequiredSourceIsNeverDropped is
// where that half is pinned.

// turnOffSource records an org admin's off switch for kind, the way the PATCH
// route does.
func turnOffSource(t *testing.T, database *sql.DB, kind string) {
	t.Helper()
	st := sqlitestore.New(database)
	if _, err := st.OrgEventSources.SetDisabled(
		context.Background(), runmode.LocalDefaultOrgID, kind, true, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("turn off %s: %v", kind, err)
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

// seedTurnOffFixtures stands up the Jira routing shape these tests share: a
// host, a user on the team, and a match-all assigned rule.
func seedTurnOffFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	seedHandlerFKTargets(t, database)
	setJiraHost(t, database)
	seedJiraUserOnTeam(t, database, runmode.LocalDefaultTeamID, "acct-aidan", "aidan")
	seedSystemJiraRule(t, database, runmode.LocalDefaultTeamID, domain.EventJiraIssueAssigned)
}

// TestHandleEvent_TurnedOffSource_RecordsButCreatesNoTask is the router's half
// of the off switch: the event is recorded — the append-only log stays an
// honest record of what happened while the source was off — and nothing
// ephemeral is derived from it. Turning it back on restores task creation on
// the next event, with no replay of what was dropped (forward-only, like every
// other tracking change).
func TestHandleEvent_TurnedOffSource_RecordsButCreatesNoTask(t *testing.T) {
	database := newTestDB(t)
	seedTurnOffFixtures(t, database)

	turnOffSource(t, database, eventsource.KindJira)
	entityID := jiraEntity(t, database, "SKY-off")
	emitJiraAssigned(gatedRouter(t, database), entityID, "acct-aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("turned-off source: got %d active tasks, want 0", len(active))
	}
	var eventCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM events WHERE entity_id = ?`, entityID).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("events recorded = %d, want 1 — a turned-off source's events are still logged", eventCount)
	}

	// Back on. A fresh router, because the gate caches per org and the
	// production invalidation (in process or over tf_ctl) is what a live one
	// would receive.
	st := sqlitestore.New(database)
	if _, err := st.OrgEventSources.SetDisabled(context.Background(), runmode.LocalDefaultOrgID, eventsource.KindJira, false, ""); err != nil {
		t.Fatalf("turn back on: %v", err)
	}
	emitJiraAssigned(gatedRouter(t, database), entityID, "acct-aidan")

	active, err = testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks after turning back on: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("after turning the source back on: got %d active tasks, want 1", len(active))
	}
}

// TestHandleEvent_TurnedOffSource_LeavesExistingTasksAlone pins the
// forward-only half: turning a source off stops NEW events minting tasks and
// touches nothing that already exists. A task is durable work that may have an
// open PR and agent memory behind it, so a config change never destroys one.
func TestHandleEvent_TurnedOffSource_LeavesExistingTasksAlone(t *testing.T) {
	database := newTestDB(t)
	seedTurnOffFixtures(t, database)

	entityID := jiraEntity(t, database, "SKY-preexisting")
	emitJiraAssigned(gatedRouter(t, database), entityID, "acct-aidan")
	before, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil || len(before) != 1 {
		t.Fatalf("precondition: tasks=%d err=%v, want 1 task", len(before), err)
	}

	turnOffSource(t, database, eventsource.KindJira)
	emitJiraAssigned(gatedRouter(t, database), entityID, "acct-aidan")

	after, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Errorf("after turning the source off: tasks=%d, want the one pre-existing task %s intact", len(after), before[0].ID)
	}
}

// TestHandleEvent_TurnedOffSource_PublishesSourceDisabled pins that the drop is
// observable rather than silent: exactly one disposition sentinel, reporting
// source_disabled — distinct from frozen, which is a licence fault nobody in
// the org chose.
func TestHandleEvent_TurnedOffSource_PublishesSourceDisabled(t *testing.T) {
	database := newTestDB(t)
	seedTurnOffFixtures(t, database)
	turnOffSource(t, database, eventsource.KindJira)

	router := gatedRouter(t, database)
	pub := &fakeDispositionPublisher{}
	router.SetEventPublisher(pub)

	emitJiraAssigned(router, jiraEntity(t, database, "SKY-disp"), "acct-aidan")

	got := pub.eventsCopy()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1", len(got))
	}
	if meta := decodeDisposition(t, got[0].MetadataJSON); meta.Disposition != events.DispositionSourceDisabled {
		t.Errorf("disposition = %q, want %q", meta.Disposition, events.DispositionSourceDisabled)
	}
}

// TestHandleEvent_TurnedOffSource_IsPerSource is the negative control: turning
// Jira off must not stop GitHub routing.
func TestHandleEvent_TurnedOffSource_IsPerSource(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)
	turnOffSource(t, database, eventsource.KindJira)

	entityID := reviewEntity(t, database, "owner/repo#other-source")
	emitCI(gatedRouter(t, database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("turning jira off changed github routing: got %d active tasks, want 1", len(active))
	}
}

// TestHandleEvent_RequiredSourceIsNeverDropped is the structural half of "you
// cannot turn GitHub off". The route refuses to write this row, so the only way
// to get one is by hand — and the gate still has to route, because a row that
// silently stopped every PR event would take the product down with no
// mechanism visible from the UI that says why.
func TestHandleEvent_RequiredSourceIsNeverDropped(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	setReviewHost(t, database)
	seedUserOnTeam(t, database, runmode.LocalDefaultTeamID, "aidan")
	seedSystemCIRule(t, database, runmode.LocalDefaultTeamID)

	turnOffSource(t, database, eventsource.KindGitHub)
	entityID := reviewEntity(t, database, "owner/repo#required")
	emitCI(gatedRouter(t, database), entityID, "aidan")

	active, err := testTaskStore(database).FindActiveByEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("a hand-written github row dropped events: got %d active tasks, want 1", len(active))
	}
}

// erroringSourceStore fails every policy read, standing in for a transient
// database fault while the router is deciding whether a source is turned off.
type erroringSourceStore struct{ dbpkg.OrgEventSourceStore }

func (erroringSourceStore) ListDisabledSystem(context.Context, string) ([]string, error) {
	return nil, errors.New("boom: org_event_sources read failed")
}

// TestHandleEvent_SourcePolicyReadFails_ErrorsRatherThanRoutes pins the
// fail-closed choice. The drain worker requeues an errored event, so a
// transient read costs a retry; answering "not turned off" on a failed read
// would mint exactly the tasks the switch exists to prevent, and nothing would
// ever notice.
func TestHandleEvent_SourcePolicyReadFails_ErrorsRatherThanRoutes(t *testing.T) {
	database := newTestDB(t)
	seedTurnOffFixtures(t, database)

	router := reviewRouter(database)
	router.SetEventSourceGate(erroringSourceStore{})
	entityID := jiraEntity(t, database, "SKY-gatefault")

	err := router.HandleEvent(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EventType: domain.EventJiraIssueAssigned,
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

// TestSourceGate_RequiredSourceSkipsTheStore pins the short-circuit's other
// half: a GitHub event costs no policy read at all. That is the hot path — one
// query per event for a row that can never say yes would be a tax on every
// repository event this deployment handles.
func TestSourceGate_RequiredSourceSkipsTheStore(t *testing.T) {
	gate := newSourceGate(erroringSourceStore{})

	off, err := gate.disabled(context.Background(), runmode.LocalDefaultOrgID, eventsource.KindGitHub)
	if err != nil || off {
		t.Fatalf("disabled(github) = %v, %v; want false, nil without reading the store", off, err)
	}
}

// TestSourceGate_TTLExpiryRereads pins the cache's floor on correctness. The
// tf_ctl invalidation normally beats the TTL, but it is lossy by contract — a
// message published in a holderless gap reaches nobody — so the TTL is what
// bounds how long a DROPPED invalidation can keep events flowing for a source
// an admin turned off.
func TestSourceGate_TTLExpiryRereads(t *testing.T) {
	database := newTestDB(t)
	st := sqlitestore.New(database)
	now := time.Now()
	gate := newSourceGate(st.OrgEventSources)
	gate.now = func() time.Time { return now }

	ctx := context.Background()
	if off, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, eventsource.KindJira); err != nil || off {
		t.Fatalf("precondition: disabled=%v err=%v, want false", off, err)
	}
	turnOffSource(t, database, eventsource.KindJira)

	// Still inside the TTL: the cached answer stands, which is the whole point
	// of caching a row that changes about once a quarter.
	if off, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, eventsource.KindJira); err != nil || off {
		t.Errorf("within TTL: disabled=%v err=%v, want the cached false", off, err)
	}
	now = now.Add(sourceGateTTL + time.Second)
	if off, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, eventsource.KindJira); err != nil || !off {
		t.Errorf("past TTL: disabled=%v err=%v, want a fresh true", off, err)
	}
}

// TestSourceGate_InvalidateDropsTheCachedAnswer pins the fast path the relay
// drives: an admin's write reaches the brain and the next event re-reads,
// rather than waiting out the TTL.
func TestSourceGate_InvalidateDropsTheCachedAnswer(t *testing.T) {
	database := newTestDB(t)
	gate := newSourceGate(sqlitestore.New(database).OrgEventSources)
	ctx := context.Background()

	if _, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, eventsource.KindJira); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	turnOffSource(t, database, eventsource.KindJira)
	gate.invalidate(runmode.LocalDefaultOrgID)

	if off, err := gate.disabled(ctx, runmode.LocalDefaultOrgID, eventsource.KindJira); err != nil || !off {
		t.Errorf("after invalidate: disabled=%v err=%v, want true without waiting out the TTL", off, err)
	}
}
