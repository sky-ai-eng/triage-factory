package routing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// testExecutorID is the owner every test router claims under. The claim
// refuses an empty owner, so a router that drains needs one.
const testExecutorID = "test-router"

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
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	r.SetEventQueue(st.EventQueue)
	r.SetExecutorID(testExecutorID, 1)
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
	}, ""); err != nil {
		t.Fatalf("enqueue ci_check_failed for %s: %v", entityID, err)
	}
}

// newEntity creates a PR entity for the queue tests and returns its id.
func newEntity(t *testing.T, database *sql.DB, sourceID string) string {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID,
		"github", sourceID, "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity %s: %v", sourceID, err)
	}
	return entity.ID
}

// expireLease rewinds a leased row's lease_expires_at into the past, standing
// in for a holder that died without a terminal write. Raw SQL: nothing in the
// store writes the column except a claim and a renewal, and a reclaim can
// only be exercised against a lease that has run out.
func expireLease(t *testing.T, database *sql.DB, queueID int64) {
	t.Helper()
	res, err := database.Exec(`UPDATE event_queue SET lease_expires_at = strftime('%Y-%m-%d %H:%M:%f','now','-1 hours') WHERE id = ? AND status = 'leased'`, queueID)
	if err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("expire lease touched %d rows, want 1", n)
	}
}

// ripenQueue clears every ready row's retry time, so a replay a test drives
// "after recovery" does not wait out the kind's backoff. Production timing
// is the scan tick's business, not a test's.
func ripenQueue(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`UPDATE event_queue SET next_attempt_at = NULL WHERE status = 'ready'`); err != nil {
		t.Fatalf("ripen queue: %v", err)
	}
}

// requestCancel records a cancellation request on a row, which the store
// exposes no path for yet.
func requestCancel(t *testing.T, database *sql.DB, queueID int64) {
	t.Helper()
	if _, err := database.Exec(`UPDATE event_queue SET cancel_requested_at = strftime('%Y-%m-%d %H:%M:%f','now'), cancel_requested_by = 'operator', cancel_reason = 'test' WHERE id = ?`, queueID); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
}

// claimOneRow claims exactly one row under owner, straight through the store.
func claimOneRow(t *testing.T, database *sql.DB, owner string) db.ClaimedEvent {
	t.Helper()
	batch, err := sqlitestore.New(database).EventQueue.Claim(context.Background(), workitem.Owner{ID: owner, Epoch: 1}, 1)
	if err != nil {
		t.Fatalf("claim as %s: %v", owner, err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("claim as %s returned %d rows, want 1", owner, len(batch.Events))
	}
	return batch.Events[0]
}

func countRows(t *testing.T, database *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

// queueRowByID reads one queue row's block columns.
func queueRowByID(t *testing.T, database *sql.DB, id int64) domain.QueuedEvent {
	t.Helper()
	var qe domain.QueuedEvent
	var lastOutcome, lastError sql.NullString
	if err := database.QueryRow(`SELECT id, status, attempt, lease_generation, last_outcome, last_error FROM event_queue WHERE id = ?`, id).
		Scan(&qe.ID, &qe.Status, &qe.Attempt, &qe.LeaseGeneration, &lastOutcome, &lastError); err != nil {
		t.Fatalf("read queue row %d: %v", id, err)
	}
	qe.LastOutcome, qe.LastError = lastOutcome.String, lastError.String
	return qe
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

	const n = 300 // > the old 256 bus buffer, on purpose
	for i := 0; i < n; i++ {
		enqueueCIFailed(t, database, newEntity(t, database, fmt.Sprintf("owner/repo#%d", i)))
	}

	// Before draining, every event is durably queued (this is what the
	// lossy bus could not guarantee).
	if ready := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='ready'`); ready != n {
		t.Fatalf("ready queue rows before drain = %d, want %d", ready, n)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

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
// survives a process restart, and that a row left leased by a crash is
// recovered by an ordinary claim once its lease expires. We enqueue three
// events, claim one (simulating a crash after claim, before mark-done),
// then simulate a restart: a fresh Router over the same DB drains with no
// boot reset of its own. All three must route — none lost.
func TestEventQueue_SurvivesRestart(t *testing.T) {
	database := newTestDB(t)
	_ = newQueueWorkerRouter(t, database)

	var entityIDs []string
	for i := 0; i < 3; i++ {
		id := newEntity(t, database, fmt.Sprintf("owner/repo#restart-%d", i))
		entityIDs = append(entityIDs, id)
		enqueueCIFailed(t, database, id)
	}

	// Simulate a crash mid-process: claim one row and "die" before routing
	// or marking it done. Until its lease runs out the row is still the
	// dead holder's, so a fresh worker leaves it alone.
	crashed := claimOneRow(t, database, "restart-instance")
	r2 := newQueueWorkerRouterNoSeed(t, database)
	if err := r2.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain with a live lease: %v", err)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 2 {
		t.Fatalf("done rows with one lease still live = %d, want 2", done)
	}
	if got := queueRowByID(t, database, crashed.Event.ID); got.Status != domain.QueuedEventStatusLeased {
		t.Fatalf("the crashed holder's row = %q, want still leased until its lease expires", got.Status)
	}

	// The lease runs out; the next drain reclaims and routes it.
	expireLease(t, database, crashed.Event.ID)
	if err := r2.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain after the lease expired: %v", err)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 3 {
		t.Errorf("done queue rows = %d, want 3 (buffered work must survive restart)", done)
	}
	if got := queueRowByID(t, database, crashed.Event.ID); got.LeaseGeneration != crashed.Receipt.LeaseGeneration+1 || got.Attempt != 2 {
		t.Errorf("reclaimed row generation=%d attempt=%d, want %d/2", got.LeaseGeneration, got.Attempt, crashed.Receipt.LeaseGeneration+1)
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

// newQueueWorkerRouterNoSeed is newQueueWorkerRouter over a database whose
// handlers are already seeded — a second router on the same file.
func newQueueWorkerRouterNoSeed(t *testing.T, database *sql.DB) *Router {
	t.Helper()
	st := sqlitestore.New(database)
	r := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, websocket.NewHub())
	r.SetEventQueue(st.EventQueue)
	r.SetExecutorID(testExecutorID+"-2", 2)
	return r
}

// TestEventQueue_ExpiredLease_RoutesAfterOwnerReplaced is the acceptance
// test for the pod-replacement hole: a pod that is replaced rather than
// rebooted — scale-down, a fresh instance id after the brain lease moves —
// never comes back for its claimed rows. Under the contract that needs no
// boot and no sweep: the lease expires, the next claim reclaims, and the
// replay is dedup-safe — exactly one task.
func TestEventQueue_ExpiredLease_RoutesAfterOwnerReplaced(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	ctx := context.Background()

	entityID := newEntity(t, database, "owner/repo#stale")
	enqueueCIFailed(t, database, entityID)

	// A pod claims the row and never comes back under this identity.
	claimed := claimOneRow(t, database, "vanished-pod")
	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drain with a live lease: %v", err)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks`); tasks != 0 {
		t.Fatalf("tasks = %d before the lease expired, want 0 (a leased row is not claimable)", tasks)
	}

	expireLease(t, database, claimed.Event.ID)
	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drain after the lease expired: %v", err)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 1 {
		t.Errorf("done queue rows = %d, want 1 (the reclaimed row must route)", done)
	}
	active, err := testTaskStore(database).FindActiveByEntity(ctx, runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("entity has %d tasks, want exactly 1", len(active))
	}
}

// TestEventQueue_LiveLease_IsNotReclaimed is the negative space: a claim
// that is merely in progress must be untouchable, or the reclaim becomes a
// source of double routing rather than a guard against loss.
func TestEventQueue_LiveLease_IsNotReclaimed(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	ctx := context.Background()

	entityID := newEntity(t, database, "owner/repo#fresh")
	enqueueCIFailed(t, database, entityID)
	claimed := claimOneRow(t, database, "live-pod")

	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := queueRowByID(t, database, claimed.Event.ID); got.Status != domain.QueuedEventStatusLeased || got.LeaseGeneration != claimed.Receipt.LeaseGeneration {
		t.Errorf("row = %+v, want still leased under the live holder's generation", got)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks`); tasks != 0 {
		t.Errorf("tasks = %d, want 0 — the live claim's row must not have been re-routed", tasks)
	}
}

// TestEventQueue_ReclaimKeepsAttemptBudget pins that reclaiming cannot turn
// a poison row into a permanent loop. Every claim charges the budget,
// reclaims included, so a row whose holders keep dying parks at claim once
// the budget is spent instead of being reclaimed forever.
func TestEventQueue_ReclaimKeepsAttemptBudget(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	ctx := context.Background()
	st := sqlitestore.New(database)

	entityID := newEntity(t, database, "owner/repo#poison")
	enqueueCIFailed(t, database, entityID)

	// Burn the retry budget the way repeated transient failures would.
	budget := eventQueueKind.Policy.MaxAttempts
	var last db.ClaimedEvent
	for i := 1; i < budget; i++ {
		last = claimOneRow(t, database, "flaky-pod")
		if parked, err := st.EventQueue.Requeue(ctx, last.Receipt, workitem.OutcomeTransient, errors.New("transient")); err != nil || parked {
			t.Fatalf("requeue %d: parked=%v err=%v", i, parked, err)
		}
		ripenQueue(t, database)
	}

	// The last claim charges the final attempt, then the owner vanishes.
	last = claimOneRow(t, database, "flaky-pod")
	if last.Receipt.Attempt != budget {
		t.Fatalf("final claim charged attempt %d, want %d", last.Receipt.Attempt, budget)
	}
	expireLease(t, database, last.Event.ID)

	// Over budget: the reclaim parks at claim rather than routing.
	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	got := queueRowByID(t, database, last.Event.ID)
	if got.Status != domain.QueuedEventStatusParked || got.Attempt != budget || got.LastOutcome != "transient" {
		t.Errorf("row = %+v, want parked at the budget under the failure that spent it", got)
	}
	if tasks := countRows(t, database, `SELECT COUNT(*) FROM tasks`); tasks != 0 {
		t.Errorf("tasks = %d, want 0 — a spent row must park, not route", tasks)
	}
	// And a parked row is terminal — a later drain never picks it up.
	if err := r.drainEventQueue(ctx); err != nil {
		t.Fatalf("drainEventQueue after the park: %v", err)
	}
	if got := queueRowByID(t, database, last.Event.ID); got.Status != domain.QueuedEventStatusParked {
		t.Errorf("row after a second drain = %q, want the parked row untouched", got.Status)
	}
}

// TestEventQueue_NoDoubleTaskOnReclaim pins that at-least-once delivery
// is safe: an event reprocessed after a crash-mid-process reclaim must
// not create a second task. The tasks partial-unique dedup index is the
// guard. We route an event (creating its task), then — without marking
// the row done — let its lease expire and drain again, forcing a second
// routing pass over the same event. Exactly one task must exist.
func TestEventQueue_NoDoubleTaskOnReclaim(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)

	entityID := newEntity(t, database, "owner/repo#reclaim")
	enqueueCIFailed(t, database, entityID)

	// First pass: claim + route, but DON'T mark done — simulating a crash
	// after the routing side effects committed, before MarkDone.
	claimed := claimOneRow(t, database, "reclaim-instance")
	ev, err := st.Events.GetSystem(context.Background(), claimed.Event.OrgID, claimed.Event.EventID)
	if err != nil || ev == nil {
		t.Fatalf("load event: ev=%v err=%v", ev, err)
	}
	if err := r.HandleEvent(context.Background(), *ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if got := countRows(t, database, `SELECT COUNT(*) FROM tasks`); got != 1 {
		t.Fatalf("after first route, tasks = %d, want 1", got)
	}

	// The lease runs out; the drain reclaims and reprocesses the same
	// event — routing runs a second time.
	expireLease(t, database, claimed.Event.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if got := countRows(t, database, `SELECT COUNT(*) FROM tasks`); got != 1 {
		t.Errorf("after reclaim + reprocess, tasks = %d, want 1 (dedup index must prevent a double)", got)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 1 {
		t.Errorf("queue row should be done after the second pass, got done=%d", done)
	}
}

// TestDrainEventQueue_FailingRowDoesNotBlockThePass is the acceptance test
// for the head-of-line fix: one persistently failing row and ten healthy
// rows behind it, and all ten route within one pass. The failing row's
// requeue carries a retry time, so the claim skips it instead of handing it
// straight back.
func TestDrainEventQueue_FailingRowDoesNotBlockThePass(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	// The first entity load fails; the outage's budget is the failing row's
	// alone, since it is the oldest row and is claimed first.
	r.entities = outageEntityStore{EntityStore: sqlitestore.New(database).Entities, o: &outage{remaining: 1}}

	failing := newEntity(t, database, "owner/repo#failing")
	enqueueCIFailed(t, database, failing)
	var healthy []string
	for i := 0; i < 10; i++ {
		id := newEntity(t, database, fmt.Sprintf("owner/repo#healthy-%d", i))
		healthy = append(healthy, id)
		enqueueCIFailed(t, database, id)
	}

	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 10 {
		t.Errorf("done rows after one pass = %d, want all ten healthy rows", done)
	}
	for _, id := range healthy {
		if n := activeTaskCount(t, database, id); n != 1 {
			t.Errorf("healthy entity %s has %d tasks, want 1", id, n)
		}
	}
	var status string
	var nextAt sql.NullString
	if err := database.QueryRow(`SELECT status, next_attempt_at FROM event_queue WHERE entity_id = ?`, failing).Scan(&status, &nextAt); err != nil {
		t.Fatalf("read the failing row: %v", err)
	}
	if status != domain.QueuedEventStatusReady || !nextAt.Valid {
		t.Errorf("failing row = (%s, next_attempt_at %v), want ready with a backoff", status, nextAt)
	}
	if n := activeTaskCount(t, database, failing); n != 0 {
		t.Errorf("failing entity has %d tasks, want 0", n)
	}
}

// blockingEntityStore holds an entity read until the unit's context is done,
// then returns its error — a dependency that hangs rather than fails.
type blockingEntityStore struct{ db.EntityStore }

func (blockingEntityStore) GetSystem(ctx context.Context, _, _ string) (*domain.Entity, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestProcessQueuedEvent_UnitDeadline_RequeuesAsDeadline pins the deadline
// outcome: a unit that outlives the kind's UnitDeadline is cut off by its
// context and requeued under the deadline outcome, with the terminal write
// landing on its own context rather than the expired one.
func TestProcessQueuedEvent_UnitDeadline_RequeuesAsDeadline(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	r.entities = blockingEntityStore{EntityStore: sqlitestore.New(database).Entities}

	saved := eventQueueKind.Policy.UnitDeadline
	eventQueueKind.Policy.UnitDeadline = 30 * time.Millisecond
	t.Cleanup(func() { eventQueueKind.Policy.UnitDeadline = saved })

	entityID := newEntity(t, database, "owner/repo#slow")
	enqueueCIFailed(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	var status, outcome string
	var lastErr sql.NullString
	if err := database.QueryRow(`SELECT status, last_outcome, last_error FROM event_queue`).Scan(&status, &outcome, &lastErr); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if status != domain.QueuedEventStatusReady || outcome != string(workitem.OutcomeDeadline) {
		t.Errorf("row = (%s, %s), want ready under the deadline outcome", status, outcome)
	}
	if !lastErr.Valid || lastErr.String == "" {
		t.Error("last_error is empty; a deadline requeue must record the cause")
	}
}

// nilEventStore answers every load with no event — the shape of a queue row
// whose events row is gone.
type nilEventStore struct{ db.EventStore }

func (nilEventStore) GetSystem(context.Context, string, string) (*domain.Event, error) {
	return nil, nil
}

// TestProcessQueuedEvent_MissingEvent_ParksPermanent pins that a row whose
// events row is gone parks at once with a typed reason instead of burning
// four retries on the same answer.
func TestProcessQueuedEvent_MissingEvent_ParksPermanent(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	r.events = nilEventStore{EventStore: sqlitestore.New(database).Events}

	enqueueCIFailed(t, database, newEntity(t, database, "owner/repo#gone"))
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	var status, outcome, lastErr string
	var attempt int
	if err := database.QueryRow(`SELECT status, attempt, last_outcome, last_error FROM event_queue`).Scan(&status, &attempt, &outcome, &lastErr); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if status != domain.QueuedEventStatusParked || attempt != 1 || outcome != string(workitem.OutcomePermanent) {
		t.Errorf("row = (%s, attempt %d, %s), want parked permanent on the first attempt", status, attempt, outcome)
	}
	if lastErr != "event row not found" {
		t.Errorf("last_error = %q", lastErr)
	}
}

// leaseLosingQueue reports every renewal as lost, standing in for a holder
// whose lease was taken over between its claim and its first fenced write.
type leaseLosingQueue struct{ db.EventQueueStore }

func (leaseLosingQueue) RenewLease(context.Context, workitem.Receipt) (workitem.Receipt, error) {
	return workitem.Receipt{}, workitem.ErrLeaseLost
}

// TestProcessQueuedEvent_LeaseLostBeforeRenewal_WritesNothing pins the
// renewal as the fence check: a unit whose lease is gone before it starts
// routes nothing and writes nothing — the row is its next holder's.
func TestProcessQueuedEvent_LeaseLostBeforeRenewal_WritesNothing(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	r.SetEventQueue(leaseLosingQueue{st.EventQueue})

	entityID := newEntity(t, database, "owner/repo#lost")
	enqueueCIFailed(t, database, entityID)
	claimed := claimOneRow(t, database, "successor")
	before := queueRowByID(t, database, claimed.Event.ID)

	r.processQueuedEvent(context.Background(), claimed)

	if after := queueRowByID(t, database, claimed.Event.ID); after != before {
		t.Errorf("row changed under a lost lease: %+v -> %+v", before, after)
	}
	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Errorf("tasks = %d, want 0 — a unit with no lease must not route", n)
	}
}

// markDoneLeaseLosingQueue expires the lease just before the terminal
// write, standing in for a takeover that landed after the domain writes
// committed and before MarkDone.
type markDoneLeaseLosingQueue struct {
	db.EventQueueStore
	t        *testing.T
	database *sql.DB
}

func (q markDoneLeaseLosingQueue) MarkDone(ctx context.Context, r workitem.Receipt) error {
	expireLease(q.t, q.database, r.ItemID)
	return q.EventQueueStore.MarkDone(ctx, r)
}

// TestProcessQueuedEvent_MarkDoneLoses_SuccessorReplayIsNoOp pins the
// crash-table row "after routing, before MarkDone": the terminal write
// loses its lease, the routing side effects stay committed, and the
// successor's replay converges through the fences to one task and one
// done row.
func TestProcessQueuedEvent_MarkDoneLoses_SuccessorReplayIsNoOp(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	st := sqlitestore.New(database)
	r.SetEventQueue(markDoneLeaseLosingQueue{EventQueueStore: st.EventQueue, t: t, database: database})

	entityID := newEntity(t, database, "owner/repo#markdone-lost")
	enqueueCIFailed(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Fatalf("tasks after the first pass = %d, want 1 — the domain writes committed", n)
	}
	if status := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='leased'`); status != 1 {
		t.Fatalf("leased rows after a lost mark done = %d, want the row left for its successor", status)
	}

	// The successor reclaims and replays.
	r.SetEventQueue(st.EventQueue)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("successor drain: %v", err)
	}
	if n := activeTaskCount(t, database, entityID); n != 1 {
		t.Errorf("tasks after the replay = %d, want still 1", n)
	}
	if done := countRows(t, database, `SELECT COUNT(*) FROM event_queue WHERE status='done'`); done != 1 {
		t.Errorf("done rows after the replay = %d, want 1", done)
	}
}

// TestEventQueue_CancellationRequests_SettleAtClaimAndRenewal pins §1.7 for
// this kind: a request on a ready row is settled by the next claim and the
// event never routes; a request on a leased row is observed by the holder's
// renewal, which is the first thing a unit does.
func TestEventQueue_CancellationRequests_SettleAtClaimAndRenewal(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)

	readyEntity := newEntity(t, database, "owner/repo#cancel-ready")
	enqueueCIFailed(t, database, readyEntity)
	var readyID int64
	if err := database.QueryRow(`SELECT id FROM event_queue WHERE entity_id = ?`, readyEntity).Scan(&readyID); err != nil {
		t.Fatalf("read row: %v", err)
	}
	requestCancel(t, database, readyID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	if got := queueRowByID(t, database, readyID); got.Status != domain.QueuedEventStatusCancelled || got.Attempt != 0 {
		t.Errorf("ready row = %+v, want settled cancelled at claim with no attempt charged", got)
	}
	if n := activeTaskCount(t, database, readyEntity); n != 0 {
		t.Errorf("tasks = %d, want 0 — a cancelled event never routes", n)
	}

	leasedEntity := newEntity(t, database, "owner/repo#cancel-leased")
	enqueueCIFailed(t, database, leasedEntity)
	claimed := claimOneRow(t, database, testExecutorID)
	requestCancel(t, database, claimed.Event.ID)
	r.processQueuedEvent(context.Background(), claimed)
	if got := queueRowByID(t, database, claimed.Event.ID); got.Status != domain.QueuedEventStatusCancelled {
		t.Errorf("leased row = %+v, want settled cancelled at renewal", got)
	}
	if n := activeTaskCount(t, database, leasedEntity); n != 0 {
		t.Errorf("tasks = %d, want 0 — the renewal observed the request before routing", n)
	}
}
