package ingest_test

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/ingest"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func newIngestTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}

func seedIngestEntity(t *testing.T, conn *sql.DB) string {
	t.Helper()
	id := "e-" + uuid.New().String()[:8]
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url)
		VALUES (?, 'github', ?, 'pr', 'PR', '')
	`, id, "owner/repo#"+id); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	return id
}

// captureBus subscribes a handler that forwards delivered events onto a
// buffered channel so the test can await the (asynchronous) bus fan-out.
func captureBus(t *testing.T) (*eventbus.Bus, <-chan domain.Event) {
	t.Helper()
	bus := eventbus.New()
	got := make(chan domain.Event, 8)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "capture",
		Handle: func(e domain.Event) { got <- e },
	})
	return bus, got
}

func awaitEvent(t *testing.T, ch <-chan domain.Event) domain.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bus delivery")
		return domain.Event{}
	}
}

func countEventQueue(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM event_queue`).Scan(&n); err != nil {
		t.Fatalf("count event_queue: %v", err)
	}
	return n
}

// seedEventCatalogRow inserts the events_catalog row eventType needs to
// satisfy the events.event_type FK the durable enqueue's recordEvent write
// hits. Production registers out-of-core event types via the gated
// catalog-upsert (a sibling ticket); this test seeds it directly.
func seedEventCatalogRow(t *testing.T, conn *sql.DB, eventType string) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT OR IGNORE INTO events_catalog (id, source, category, label, description)
		VALUES (?, 'fake', 'thing', 'Fake', 'fake-source test fixture')
	`, eventType); err != nil {
		t.Fatalf("seed events_catalog for %s: %v", eventType, err)
	}
}

// TestIngestor_RouterBoundEvent_DurablyEnqueuedAndPublished pins the core
// split: a github:/jira: event is durably enqueued (events row + queue
// row), the drainer is woken, AND the event still reaches the bus for
// cosmetic subscribers — now carrying the id the enqueue minted.
func TestIngestor_RouterBoundEvent_DurablyEnqueuedAndPublished(t *testing.T) {
	conn := newIngestTestDB(t)
	entityID := seedIngestEntity(t, conn)
	queue := sqlitestore.New(conn).EventQueue
	bus, got := captureBus(t)

	var wakes int32
	ing := ingest.New(bus, queue, func() { atomic.AddInt32(&wakes, 1) })

	eid := entityID
	ing.Publish(context.Background(), domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRCICheckFailed,
		DedupKey:     "build",
		MetadataJSON: `{"check_name":"build"}`,
	})

	// Durable path is synchronous: one pending queue row with a real
	// event id (the FK proves the events audit row landed too).
	rows, err := queue.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 queue row, got %d", len(rows))
	}
	if rows[0].Status != domain.QueuedEventStatusPending {
		t.Errorf("queue row status = %q, want pending", rows[0].Status)
	}
	if rows[0].EventID == "" {
		t.Errorf("queue row has empty event_id")
	}
	if n := atomic.LoadInt32(&wakes); n != 1 {
		t.Errorf("wake called %d times, want 1", n)
	}

	// Bus fan-out (async) still happens, carrying the minted id so the WS
	// feed and the queue agree.
	e := awaitEvent(t, got)
	if e.EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("bus event type = %q, want ci_check_failed", e.EventType)
	}
	if e.ID != rows[0].EventID {
		t.Errorf("bus event id = %q, want %q (must match the enqueued row)", e.ID, rows[0].EventID)
	}
}

// TestIngestor_SystemEvent_BusOnly pins that non-router events
// (system:poll:*) are forwarded to the bus but never enqueued and never
// wake the drainer — they're coalesced signals, not durable work.
func TestIngestor_SystemEvent_BusOnly(t *testing.T) {
	conn := newIngestTestDB(t)
	queue := sqlitestore.New(conn).EventQueue
	bus, got := captureBus(t)

	var wakes int32
	ing := ingest.New(bus, queue, func() { atomic.AddInt32(&wakes, 1) })

	ing.Publish(context.Background(), domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EventType:    domain.EventSystemPollCompleted,
		MetadataJSON: `{"source":"github"}`,
	})

	if n := countEventQueue(t, conn); n != 0 {
		t.Errorf("event_queue has %d rows, want 0 (system events are bus-only)", n)
	}
	if n := atomic.LoadInt32(&wakes); n != 0 {
		t.Errorf("wake called %d times for a system event, want 0", n)
	}
	if e := awaitEvent(t, got); e.EventType != domain.EventSystemPollCompleted {
		t.Errorf("bus event type = %q, want poll_completed", e.EventType)
	}
}

// TestIngestor_NilQueue_BusOnly pins the degraded wiring (queue == nil):
// every event still reaches the bus, nothing is enqueued, no panic. This
// is the shape tracker tests get when they pass a bare bus.
func TestIngestor_NilQueue_BusOnly(t *testing.T) {
	conn := newIngestTestDB(t)
	entityID := seedIngestEntity(t, conn)
	bus, got := captureBus(t)

	ing := ingest.New(bus, nil, nil)

	eid := entityID
	ing.Publish(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckFailed,
	})

	if n := countEventQueue(t, conn); n != 0 {
		t.Errorf("event_queue has %d rows, want 0 (nil queue must not enqueue)", n)
	}
	if e := awaitEvent(t, got); e.EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("bus event type = %q, want ci_check_failed", e.EventType)
	}
}

// failingEnqueueQueue is a db.EventQueueStore whose Enqueue always errors;
// every other method is an inert stub. Used to exercise the ingest
// failure path without standing up a broken DB.
type failingEnqueueQueue struct{ err error }

func (q failingEnqueueQueue) Enqueue(context.Context, string, domain.Event, string) (string, error) {
	return "", q.err
}
func (q failingEnqueueQueue) EnqueueBatchWithSnapshotCAS(context.Context, string, string, string, int64, []domain.Event, []string) (bool, []string, error) {
	return false, nil, q.err
}
func (failingEnqueueQueue) ClaimNext(context.Context, string, int64) (*domain.QueuedEvent, error) {
	return nil, nil
}
func (failingEnqueueQueue) MarkDone(context.Context, string, int64) error           { return nil }
func (failingEnqueueQueue) MarkFailed(context.Context, string, int64, string) error { return nil }
func (failingEnqueueQueue) Requeue(context.Context, string, int64, string) error    { return nil }
func (failingEnqueueQueue) ResetProcessing(context.Context, string, int64) (int, error) {
	return 0, nil
}
func (failingEnqueueQueue) RequeueStaleProcessing(context.Context, time.Duration) (int, error) {
	return 0, nil
}
func (failingEnqueueQueue) PruneDone(context.Context, time.Time) (int, error) { return 0, nil }
func (failingEnqueueQueue) ListForEntity(context.Context, string, string) ([]domain.QueuedEvent, error) {
	return nil, nil
}
func (failingEnqueueQueue) ListFailedEvents(context.Context, string, int) ([]domain.FailedEvent, error) {
	return nil, nil
}
func (failingEnqueueQueue) RequeueFailedEvents(context.Context, string, []int64) (int, error) {
	return 0, nil
}

var _ db.EventQueueStore = failingEnqueueQueue{}

// TestIngestor_EnqueueFailure_DropsNoBusPhantom pins that when the durable
// enqueue fails, the router-bound event is dropped rather than forwarded
// to the bus — no id-less phantom live update, and the drainer isn't
// woken for work that was never queued.
func TestIngestor_EnqueueFailure_DropsNoBusPhantom(t *testing.T) {
	bus, got := captureBus(t)
	var wakes int32
	ing := ingest.New(bus, failingEnqueueQueue{err: errors.New("db down")}, func() { atomic.AddInt32(&wakes, 1) })

	eid := "entity-x"
	ing.Publish(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckFailed,
	})

	select {
	case e := <-got:
		t.Fatalf("enqueue failure should not publish to the bus, got %q", e.EventType)
	case <-time.After(150 * time.Millisecond):
		// expected: nothing delivered
	}
	if n := atomic.LoadInt32(&wakes); n != 0 {
		t.Errorf("wake called %d times on enqueue failure, want 0", n)
	}
}

// TestIngestor_RegisteredSource_DurablyEnqueued pins the routerBound seam: an
// event from a source registered with internal/routing (standing in for an
// ee/ package's "slack:" prefix) gets the same durable-outbox treatment as a
// built-in github:/jira: event — without this, a registered source's events
// would go bus-only and never reach the router via the drain worker.
func TestIngestor_RegisteredSource_DurablyEnqueued(t *testing.T) {
	t.Cleanup(routing.ResetSources)
	routing.RegisterSource("fake", routing.SourceHooks{
		ResolveOwner: func(context.Context, string, domain.Event, string) (string, []string, error) {
			return "", nil, nil // pool-only source; never consulted
		},
		TracksScope: func(context.Context, domain.Event, string) bool { return true },
	})

	conn := newIngestTestDB(t)
	entityID := seedIngestEntity(t, conn)
	seedEventCatalogRow(t, conn, "fake:x")
	queue := sqlitestore.New(conn).EventQueue
	bus, got := captureBus(t)

	var wakes int32
	ing := ingest.New(bus, queue, func() { atomic.AddInt32(&wakes, 1) })

	eid := entityID
	ing.Publish(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EntityID:  &eid,
		EventType: "fake:x",
	})

	rows, err := queue.ListForEntity(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 queue row for a registered source's event, got %d", len(rows))
	}
	if n := atomic.LoadInt32(&wakes); n != 1 {
		t.Errorf("wake called %d times, want 1", n)
	}
	if e := awaitEvent(t, got); e.EventType != "fake:x" {
		t.Errorf("bus event type = %q, want fake:x", e.EventType)
	}
}

// TestIngestor_UnregisteredSource_BusOnly pins the negative: an event whose
// source prefix has no registration (no ee/ package installed) stays
// bus-only, same as a system event — routing.RouterBound must gate the
// enqueue, not just "any non-github/jira prefix."
func TestIngestor_UnregisteredSource_BusOnly(t *testing.T) {
	routing.ResetSources() // defensive: this test's assertion depends on nothing being registered

	conn := newIngestTestDB(t)
	queue := sqlitestore.New(conn).EventQueue
	bus, got := captureBus(t)

	ing := ingest.New(bus, queue, nil)
	ing.Publish(context.Background(), domain.Event{
		OrgID:     runmode.LocalDefaultOrgID,
		EventType: "unregistered-fake:x",
	})

	if n := countEventQueue(t, conn); n != 0 {
		t.Errorf("event_queue has %d rows, want 0 (unregistered source stays bus-only)", n)
	}
	if e := awaitEvent(t, got); e.EventType != "unregistered-fake:x" {
		t.Errorf("bus event type = %q, want unregistered-fake:x", e.EventType)
	}
}

// TestIngestor_PublishPreEnqueued_FansOutWithoutEnqueueing pins the
// tracker's post-commit path: the event reaches the bus and the drainer is
// woken, but nothing is written — the caller already committed the queue
// row itself, inside the transaction that advanced the entity snapshot, so
// a second enqueue here would be a duplicate row for the same event.
func TestIngestor_PublishPreEnqueued_FansOutWithoutEnqueueing(t *testing.T) {
	conn := newIngestTestDB(t)
	entityID := seedIngestEntity(t, conn)
	queue := sqlitestore.New(conn).EventQueue
	bus, got := captureBus(t)

	var wakes int32
	ing := ingest.New(bus, queue, func() { atomic.AddInt32(&wakes, 1) })

	eid := entityID
	ing.PublishPreEnqueued(context.Background(), domain.Event{
		ID:        "already-minted-id",
		OrgID:     runmode.LocalDefaultOrgID,
		EntityID:  &eid,
		EventType: domain.EventGitHubPRCICheckFailed,
	})

	if n := countEventQueue(t, conn); n != 0 {
		t.Errorf("event_queue has %d rows, want 0 — a pre-enqueued event must not be enqueued again", n)
	}
	e := awaitEvent(t, got)
	if e.EventType != domain.EventGitHubPRCICheckFailed {
		t.Errorf("bus event type = %q, want ci_check_failed", e.EventType)
	}
	if e.ID != "already-minted-id" {
		t.Errorf("bus event id = %q, want the id the caller's enqueue minted", e.ID)
	}
	if n := atomic.LoadInt32(&wakes); n != 1 {
		t.Errorf("wake called %d times, want 1 — the committed row still needs draining", n)
	}
}
