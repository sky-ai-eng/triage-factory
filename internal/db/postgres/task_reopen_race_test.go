package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// The reopen's races, run against concurrent connections: the shared suite
// shows each order one after the other, and these show that whichever order
// the database picks, one task ends up active on the key and no row is half
// written. SQLite has one writer, so it has no interleaving to show.

// TestTaskReopen_Postgres_ConcurrentMintsReopenOnce: several events for a
// closed task's key arrive together. One mint reopens the task and the rest
// find it active; none creates a second task.
func TestTaskReopen_Postgres_ConcurrentMintsReopenOnce(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed

	orgID, userID, _ := seedPgOrgUserAgent(t, h)
	seed := newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)
	taskID := seed.Task(t)
	entityID := seed.EntityForTask(t, taskID)
	seed.BlueprintAndConversation(t, taskID, "running", "")
	if _, err := stores.Tasks.CloseSystem(ctx, orgID, taskID, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed); err != nil {
		t.Fatalf("CloseSystem: %v", err)
	}

	const writers = 6
	eventIDs := make([]string, writers)
	for i := range eventIDs {
		eventIDs[i] = seed.Event(t, taskID)
	}
	taskIDs := make([]string, writers)
	mints := make([]db.TaskMint, writers)
	errs := make([]error, writers)
	var wg, ready sync.WaitGroup
	ready.Add(writers)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			task, mint, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, ci, "", eventIDs[i], 0.5, time.Now().UTC())
			errs[i], mints[i] = err, mint
			if task != nil {
				taskIDs[i] = task.ID
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()

	reopened := 0
	for i := range writers {
		if errs[i] != nil {
			t.Fatalf("writer %d: %v", i, errs[i])
		}
		if taskIDs[i] != taskID {
			t.Errorf("writer %d got task %s, want the closed task %s", i, taskIDs[i], taskID)
		}
		switch mints[i] {
		case db.TaskReopened:
			reopened++
		case db.TaskFound:
		default:
			t.Errorf("writer %d: mint = %v, want reopened or found", i, mints[i])
		}
	}
	if reopened != 1 {
		t.Errorf("reopens = %d, want exactly 1", reopened)
	}
	var count int
	if err := h.AdminDB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE org_id = $1 AND entity_id = $2 AND event_type = $3`, orgID, entityID, ci).Scan(&count); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if count != 1 {
		t.Errorf("tasks on the key = %d, want the one reopened", count)
	}
}

// TestTaskReopen_Postgres_RacingARequeueLeavesOneActiveTask: a person
// requeues an older closed task while an event reopens the newest one. Both
// would hold the key. Whichever lands second either finds the other's task or
// is refused by the dedup index, and the task that stayed closed keeps its
// close.
func TestTaskReopen_Postgres_RacingARequeueLeavesOneActiveTask(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	const ci = domain.EventGitHubPRCICheckFailed

	orgID, userID, _ := seedPgOrgUserAgent(t, h)
	seed := newPgCloseDeferralSeeder(h.AdminDB, orgID, userID)

	const rounds = 12
	for round := range rounds {
		older := seed.Task(t)
		entityID := seed.EntityForTask(t, older)
		if _, err := stores.Tasks.CloseByUser(ctx, orgID, older, "dismissed", dbtest.TaskVersion(t, stores.Tasks, orgID, older)); err != nil {
			t.Fatalf("round %d: CloseByUser: %v", round, err)
		}
		newer := seed.TaskOnEntity(t, entityID, ci)
		seed.BlueprintAndConversation(t, newer, "running", "")
		if _, err := stores.Tasks.CloseSystem(ctx, orgID, newer, db.SiblingEventCloseReason, domain.EventGitHubPRCICheckPassed); err != nil {
			t.Fatalf("round %d: CloseSystem: %v", round, err)
		}
		olderVersion := dbtest.TaskVersion(t, stores.Tasks, orgID, older)
		eventID := seed.Event(t, older)

		var (
			wg         sync.WaitGroup
			requeued   bool
			requeueErr error
			minted     *domain.Task
			mint       db.TaskMint
			mintErr    error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			requeued, requeueErr = stores.Tasks.RequeueTask(ctx, orgID, older, olderVersion)
		}()
		go func() {
			defer wg.Done()
			<-start
			minted, mint, mintErr = stores.Tasks.FindOrCreateAtSystem(ctx, orgID, "", entityID, ci, "", eventID, 0.5, time.Now().UTC())
		}()
		close(start)
		wg.Wait()

		rows, err := h.AdminDB.Query(`
			SELECT id, status, closed_at IS NOT NULL, close_reason IS NOT NULL
			  FROM tasks WHERE org_id = $1 AND entity_id = $2 AND event_type = $3
		`, orgID, entityID, ci)
		if err != nil {
			t.Fatalf("round %d: read tasks: %v", round, err)
		}
		var active []string
		for rows.Next() {
			var id, status string
			var hasClosedAt, hasReason bool
			if err := rows.Scan(&id, &status, &hasClosedAt, &hasReason); err != nil {
				t.Fatalf("round %d: scan task: %v", round, err)
			}
			closed := status == "done" || status == "dismissed"
			if closed != hasClosedAt || closed != hasReason {
				t.Errorf("round %d: task %s is %q with closed_at set=%v, close_reason set=%v: half written", round, id, status, hasClosedAt, hasReason)
			}
			if !closed {
				active = append(active, id)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("round %d: read tasks: %v", round, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("round %d: close rows: %v", round, err)
		}

		if len(active) != 1 {
			t.Fatalf("round %d: active tasks on the key = %v, want exactly one (requeue ok=%v err=%v, mint=%v err=%v)",
				round, active, requeued, requeueErr, mint, mintErr)
		}
		// The mint answers with the task that holds the key, or fails.
		if mintErr == nil && (minted == nil || minted.ID != active[0]) {
			t.Errorf("round %d: mint returned %v (%v), want the active task %s", round, minted, mint, active[0])
		}
		// Each writer that reported success is the one whose task is active.
		if requeueErr == nil && requeued != (active[0] == older) {
			t.Errorf("round %d: requeue reported %v, but the active task is %s (older %s)", round, requeued, active[0], older)
		}
		if mintErr == nil && mint == db.TaskReopened && active[0] != newer {
			t.Errorf("round %d: mint reported a reopen, but the active task is %s (newer %s)", round, active[0], newer)
		}
		if mintErr == nil && mint == db.TaskCreated {
			t.Errorf("round %d: mint created a third task on the key", round)
		}
	}
}
