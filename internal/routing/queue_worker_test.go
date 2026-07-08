package routing

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// newQueueWorkerRouter builds a fully-wired router with the durable event
// queue set so a ci_check_failed event routed through the worker creates a
// task. The shipped (system) CI rule no longer creates an author-centric task
// on its own — owner routing scopes it — so an explicit team-owned watch rule
// is seeded too: it grants visibility (and thus task creation) regardless of
// whether the event's author resolves to a TF user, which keeps these
// queue-durability tests focused on the queue, not on identity. teamRepos is
// nil → the team↔repo gate fails open.
func newQueueWorkerRouter(t *testing.T, database *sql.DB) *Router {
	t.Helper()
	st := sqlitestore.New(database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}
	seedMatchAllCIRule(t, database, runmode.LocalDefaultTeamID)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), st.AgentRuns, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	r.SetEventQueue(st.EventQueue)
	return r
}

// enqueueCIFailed durably enqueues a ci_check_failed event for entityID
// via the EventQueueStore — the same path the production ingestor uses.
func enqueueCIFailed(t *testing.T, database *sql.DB, entityID string) {
	t.Helper()
	eid := entityID
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRCICheckFailed,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	}); err != nil {
		t.Fatalf("enqueue ci_check_failed for %s: %v", entityID, err)
	}
}

func countRows(t *testing.T, database *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// TestEventQueue_Burst_AllRouted is the acceptance test for the headline
// bug: a discovery/backfill burst that overran the old in-memory bus's
// 256-deep buffer dropped events (and their tasks) silently. With the
// durable queue there is no such ceiling — every enqueued event is
// claimed and routed. We enqueue well past 256 distinct entities and
// assert that every one produced its task and every queue row reached
// 'done' (zero drops).
func TestEventQueue_Burst_AllRouted(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	const n = 300 // > the old 256 bus buffer, on purpose
	for i := 0; i < n; i++ {
		entity, _, err := st.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID,
			"github", fmt.Sprintf("owner/repo#%d", i), "pr", "PR", "https://example.com")
		if err != nil {
			t.Fatalf("create entity %d: %v", i, err)
		}
		enqueueCIFailed(t, database, entity.ID)
	}

	// Before draining, every event is durably queued (this is what the
	// lossy bus could not guarantee).
	if pending := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='pending'`); pending != n {
		t.Fatalf("pending queue rows before drain = %d, want %d", pending, n)
	}

	r.drainEventQueue(context.Background())

	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != n {
		t.Errorf("done queue rows after drain = %d, want %d (events dropped)", done, n)
	}
	if leftover := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status!='done'`); leftover != 0 {
		t.Errorf("%d queue rows left un-done after drain, want 0", leftover)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks WHERE status='queued'`); tasks != n {
		t.Errorf("tasks created = %d, want %d (one per routed event)", tasks, n)
	}
}

// TestEventQueue_SurvivesRestart pins that buffered-but-unprocessed work
// survives a process restart, and that a row left 'processing' by a
// crash mid-process is recovered. We enqueue three events, claim one to
// 'processing' (simulating a crash after claim, before mark-done), then
// simulate a restart: a fresh Router over the same DB runs boot recovery
// (ResetProcessing) and drains. All three must route — none lost.
func TestEventQueue_SurvivesRestart(t *testing.T) {
	database := newTestDB(t)
	r1 := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	var entityIDs []string
	for i := 0; i < 3; i++ {
		entity, _, err := st.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID,
			"github", fmt.Sprintf("owner/repo#restart-%d", i), "pr", "PR", "https://example.com")
		if err != nil {
			t.Fatalf("create entity %d: %v", i, err)
		}
		entityIDs = append(entityIDs, entity.ID)
		enqueueCIFailed(t, database, entity.ID)
	}

	// Simulate a crash mid-process: claim one row (pending → processing)
	// and "die" before routing or marking it done. r1 is then abandoned.
	// "restart-instance" boot 1 stands in for r1's (unset in this test)
	// identity.
	if _, err := st.EventQueue.ClaimNext(context.Background(), "restart-instance", 1); err != nil {
		t.Fatalf("pre-crash claim: %v", err)
	}
	_ = r1

	// Restart: a fresh worker over the same DB. Boot recovery resets the
	// in-flight row back to pending (the SAME persistent instance identity,
	// a later boot epoch — TFAC-578's self-sweep); the drain then routes all
	// three.
	r2 := newQueueWorkerRouter(t, database)
	if n, err := st.EventQueue.ResetProcessing(context.Background(), "restart-instance", 2); err != nil {
		t.Fatalf("boot recovery: %v", err)
	} else if n != 1 {
		t.Errorf("ResetProcessing reset %d rows, want 1 (the in-flight claim)", n)
	}
	r2.drainEventQueue(context.Background())

	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 3 {
		t.Errorf("done queue rows = %d, want 3 (buffered work must survive restart)", done)
	}
	for _, eid := range entityIDs {
		active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, eid)
		if err != nil {
			t.Fatalf("list active for %s: %v", eid, err)
		}
		if len(active) != 1 {
			t.Errorf("entity %s has %d tasks, want 1", eid, len(active))
		}
	}
}

// TestEventQueue_NoDoubleTaskOnReclaim pins that at-least-once delivery
// is safe: an event reprocessed after a crash-mid-process re-claim must
// not create a second task. The tasks partial-unique dedup index is the
// guard. We route an event (creating its task), then — without marking
// the row done — reset it and drain again, forcing a second HandleEvent
// pass over the same event. Exactly one task must exist.
func TestEventQueue_NoDoubleTaskOnReclaim(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	entity, _, err := st.Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#reclaim", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)

	// First pass: claim + route, but DON'T mark done — simulating a crash
	// after the routing side effects committed, before MarkDone.
	qe, err := st.EventQueue.ClaimNext(context.Background(), "reclaim-instance", 1)
	if err != nil || qe == nil {
		t.Fatalf("claim: qe=%v err=%v", qe, err)
	}
	ev, err := st.Events.GetSystem(context.Background(), qe.OrgID, qe.EventID)
	if err != nil || ev == nil {
		t.Fatalf("load event: ev=%v err=%v", ev, err)
	}
	r.HandleEvent(*ev) // task created here

	if got := countRows(t, database, `SELECT COUNT(*) FROM tasks`); got != 1 {
		t.Fatalf("after first route, tasks = %d, want 1", got)
	}

	// Crash recovery re-claims the still-'processing' row, then the drain
	// reprocesses the same event — HandleEvent runs a second time.
	if _, err := st.EventQueue.ResetProcessing(context.Background(), "reclaim-instance", 2); err != nil {
		t.Fatalf("reset processing: %v", err)
	}
	r.drainEventQueue(context.Background())

	if got := countRows(t, database, `SELECT COUNT(*) FROM tasks`); got != 1 {
		t.Errorf("after re-claim + reprocess, tasks = %d, want 1 (dedup index must prevent a double)", got)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 1 {
		t.Errorf("queue row should be done after the second pass, got done=%d", done)
	}
}
