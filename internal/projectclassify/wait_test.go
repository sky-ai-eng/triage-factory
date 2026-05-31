package projectclassify

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// TestWaitFor_ReturnsImmediatelyWhenAlreadyClassified guards the
// fast-path: an entity that's already been classified should not
// trigger the runner or wait at all.
func TestWaitFor_ReturnsImmediatelyWhenAlreadyClassified(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitestore.New(database).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, entity.ID, nil, ""); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	start := time.Now()
	WaitFor(context.Background(), runner, runmode.LocalDefaultOrgID, entity.ID, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s — should have returned immediately for classified entity", elapsed)
	}
}

// TestWaitFor_TriggersRunnerOnEntry verifies that WaitFor pings the
// runner so it wakes up even if no post-poll trigger has fired for
// this entity yet. Relies on the runner being started so the trigger
// channel actually drains.
func TestWaitFor_TriggersRunnerOnEntry(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#2", "pr", "T", "https://x/2")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	// Don't Start the runner — we just want to observe that Trigger()
	// got invoked. Inspect the trigger channel directly: a buffered
	// chan with capacity 1 should have one signal queued after WaitFor.
	// Cancellable ctx so the goroutine doesn't outlive the test —
	// avoids a leaked WaitFor sleeping out its timeout in the
	// background and logging into adjacent tests.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go WaitFor(ctx, runner, runmode.LocalDefaultOrgID, entity.ID, 200*time.Millisecond)
	// Give the goroutine a moment to enter WaitFor and call Trigger.
	time.Sleep(50 * time.Millisecond)

	select {
	case <-runner.trigger:
		// expected
	default:
		t.Errorf("runner.Trigger not invoked by WaitFor")
	}
}

// TestWaitFor_HonorsTimeout verifies WaitFor returns within the
// configured budget when classification never completes.
func TestWaitFor_HonorsTimeout(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#3", "pr", "T", "https://x/3")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	// Drain any trigger so the test focuses on the timeout path.
	go func() {
		<-runner.trigger
	}()

	start := time.Now()
	WaitFor(context.Background(), runner, runmode.LocalDefaultOrgID, entity.ID, 250*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 200*time.Millisecond {
		t.Errorf("WaitFor returned in %s — too fast for a 250ms timeout", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("WaitFor took %s — far longer than the 250ms timeout", elapsed)
	}
}

// TestWaitFor_WakesOnceClassificationLands verifies the polling loop
// returns promptly once classified_at is set mid-wait.
func TestWaitFor_WakesOnceClassificationLands(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#4", "pr", "T", "https://x/4")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	go func() {
		<-runner.trigger
	}()

	// Mid-wait, mark the entity classified.
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := sqlitestore.New(database).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, entity.ID, nil, ""); err != nil {
			t.Errorf("AssignEntityProject in goroutine: %v", err)
		}
	}()

	start := time.Now()
	WaitFor(context.Background(), runner, runmode.LocalDefaultOrgID, entity.ID, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("WaitFor returned in %s — should have woken within the polling cadence after classification", elapsed)
	}
}

// TestWaitFor_ReturnsEarlyOnMissingEntity guards against the bug where
// classified() treated sql.ErrNoRows as "still unclassified" and the
// caller stalled the full timeout for a deleted/non-existent row.
//
// Intentionally no drainer goroutine on runner.trigger — WaitFor
// short-circuits BEFORE calling Trigger() for a missing entity, so a
// drainer would block forever waiting for a signal that never comes.
func TestWaitFor_ReturnsEarlyOnMissingEntity(t *testing.T) {
	database := newTestDB(t)
	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)

	// UUID-shaped id so this mirrors the cross-backend convention
	// (entity_conformance.go binds missing ids as UUIDs for the Postgres
	// uuid column); the row simply doesn't exist.
	start := time.Now()
	WaitFor(context.Background(), runner, runmode.LocalDefaultOrgID, uuid.NewString(), 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s for a missing entity — should have returned immediately", elapsed)
	}
}

// TestWaitFor_ReturnsOnContextCancel guards the spawner-shutdown path:
// when the caller's ctx is cancelled (run aborted, server shutting
// down), WaitFor must break out instead of blocking the full timeout.
func TestWaitFor_ReturnsOnContextCancel(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#5", "pr", "T", "https://x/5")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	go func() {
		<-runner.trigger
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	WaitFor(ctx, runner, runmode.LocalDefaultOrgID, entity.ID, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("WaitFor took %s after ctx cancel — should have returned within the next poll tick", elapsed)
	}
}

// TestWaitFor_PreCancelledCtxReturnsWithoutTrigger guards the entry-time
// cancellation guard: a WaitFor entered with an already-cancelled context
// must return immediately AND must not kick the classifier — waking the
// runner on a shutdown path is pointless work. Intentionally no drainer
// goroutine on runner.trigger; we assert the channel is empty afterward.
func TestWaitFor_PreCancelledCtxReturnsWithoutTrigger(t *testing.T) {
	database := newTestDB(t)
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#6", "pr", "T", "https://x/6")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before entry

	start := time.Now()
	WaitFor(ctx, runner, runmode.LocalDefaultOrgID, entity.ID, 5*time.Second)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("WaitFor took %s for a pre-cancelled ctx — should have returned immediately", elapsed)
	}

	// The entry guard returns before runner.Trigger(), so the buffered
	// trigger channel must still be empty.
	select {
	case <-runner.trigger:
		t.Errorf("WaitFor kicked the classifier on a pre-cancelled ctx — the entry guard should skip Trigger()")
	default:
	}
}

// silence unused-import warning when domain is not directly used in
// trivial paths above.
var _ = domain.Entity{}
var _ atomic.Int32
