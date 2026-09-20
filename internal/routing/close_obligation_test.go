package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The close phase is a routing obligation, not a best-effort tail: the
// transitions it acts on emit exactly once (the tracker writes the snapshot
// before publishing, so the next cycle diffs MERGED against MERGED and produces
// nothing), and nothing re-derives a close that was dropped. These tests pin
// that a failure inside it leaves the event on the queue, that one bad row
// doesn't hold up its siblings, and that the replay lands exactly once.

// enqueueCIFailedCheck durably enqueues a ci_check_failed event keyed on a
// named check, so a caller can stand up SEVERAL live tasks of the same type on
// one entity (the dedup key is what splits them) and watch the terminal close
// reap them as a set.
func enqueueCIFailedCheck(t *testing.T, database *sql.DB, entityID, checkName string) {
	t.Helper()
	eid := entityID
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRCICheckFailed,
		DedupKey:     checkName,
		MetadataJSON: `{"check_name":"` + checkName + `"}`,
	}, ""); err != nil {
		t.Fatalf("enqueue ci_check_failed(%s): %v", checkName, err)
	}
}

// enqueueMerged durably enqueues the terminating github:pr:merged event — the
// one that closes every in-flight task on the entity and flips the entity
// itself.
func enqueueMerged(t *testing.T, database *sql.DB, entityID string) {
	t.Helper()
	eid := entityID
	meta, _ := json.Marshal(events.GitHubPRMergedMetadata{
		Author: "aidan", Repo: "owner/repo", PRNumber: 1, MergedBy: "aidan", HeadSHA: "abc123",
	})
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRMerged,
		MetadataJSON: string(meta),
	}, ""); err != nil {
		t.Fatalf("enqueue pr:merged: %v", err)
	}
}

// taskCloseOutageStore fails ONE named task's typed close, so a test can watch
// its siblings close on the same pass instead of being cancelled by it. The
// typed close the router performs is the combined one — task flip, audit row,
// and stop intent in a single transaction — so that is what this intercepts.
// A terminating close does not pass through here: it is one transaction on
// the entity store (closeTerminalOutageStore fails that one).
type taskCloseOutageStore struct {
	dbpkg.TaskStore
	failTaskID string
	o          *outage
}

func (s *taskCloseOutageStore) CloseWithConversationCancelIntentSystem(ctx context.Context, orgID, taskID, closeReason, closeEventType, closingEventID string) (bool, []string, error) {
	if taskID == s.failTaskID && s.o.down() {
		return false, nil, errOutage
	}
	return s.TaskStore.CloseWithConversationCancelIntentSystem(ctx, orgID, taskID, closeReason, closeEventType, closingEventID)
}

func closeAuditCount(t *testing.T, database *sql.DB, taskID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id = ? AND kind = 'closed'`, taskID).Scan(&n); err != nil {
		t.Fatalf("count close audit rows: %v", err)
	}
	return n
}

// TestCloseObligation_TerminatingCloseFails_RequeuesThenClosesOnRetry is the
// headline: the terminating close is load-bearing and, if dropped, is repaired
// only by the poll's obligation one cycle later — a merged PR whose row stays
// 'active' is polled again, its stragglers keep routing meanwhile, and the
// transition that would have closed it will never be emitted again. So the
// failure must leave the event un-consumed.
func TestCloseObligation_TerminatingCloseFails_RequeuesThenClosesOnRetry(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	o := &outage{remaining: 1}
	r.entities = closeTerminalOutageStore{EntityStore: sqlitestore.New(database).Entities, o: o}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#close-obligation", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailed(t, database, entity.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the ci event: %v", err)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 1 {
		t.Fatalf("setup: active tasks = %d, want 1", n)
	}

	// The merge arrives and the entity close fails.
	enqueueMerged(t, database, entity.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}
	status, attempts, lastErr := mergedQueueRow(t, database)
	if status != domain.QueuedEventStatusReady || attempts != 1 {
		t.Errorf("row after a failed entity close = (%s, attempts %d), want (ready, 1) — the close is owed, not done", status, attempts)
	}
	if lastErr == "" {
		t.Error("last_error is empty; a requeued row must record why the close did not land")
	}
	if got := entityState(t, database, entity.ID); got != "active" {
		t.Errorf("entity state = %q, want active — the close write failed", got)
	}

	// The store recovers and the replay finishes the job.
	ripenQueue(t, database)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if status, _, _ = mergedQueueRow(t, database); status != domain.QueuedEventStatusDone {
		t.Errorf("row after recovery = %q, want done", status)
	}
	if got := entityState(t, database, entity.ID); got != "closed" {
		t.Errorf("entity state = %q, want closed", got)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 0 {
		t.Errorf("active tasks = %d, want 0 — the merge closes every in-flight task", n)
	}
}

// TestCloseObligation_TerminatingCloseIsAllOrNothing pins the transaction
// shape. A merge that closes several tasks either closes all of them and the
// entity, or none of anything: a failure mid-way leaves every task open and
// the entity active, and the replay does the whole job once, with exactly one
// close-audit row per task.
//
// The entity NOT flipping on the failed pass is the load-bearing part: the
// closed-entity gate drops a replayed event before the close phase, so an
// entity closed with a task still open would hide that task from its own
// retry — which is why the flip and the task closes are one transaction.
func TestCloseObligation_TerminatingCloseIsAllOrNothing(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID,
		"github", "owner/repo#sibling-close", "pr", "PR", "https://example.com")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	enqueueCIFailedCheck(t, database, entity.ID, "build")
	enqueueCIFailedCheck(t, database, entity.ID, "lint")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the ci events: %v", err)
	}
	live, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || len(live) != 2 {
		t.Fatalf("setup: active tasks = %d (err %v), want 2", len(live), err)
	}

	r.entities = closeTerminalOutageStore{EntityStore: sqlitestore.New(database).Entities, o: &outage{remaining: 1}}
	enqueueMerged(t, database, entity.ID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if n := activeTaskCount(t, database, entity.ID); n != 2 {
		t.Fatalf("active tasks after the failed close = %d, want both — a terminating close lands whole or not at all", n)
	}
	if status, attempts, _ := mergedQueueRow(t, database); status != domain.QueuedEventStatusReady || attempts != 1 {
		t.Errorf("row = (%s, attempts %d), want (ready, 1)", status, attempts)
	}
	if got := entityState(t, database, entity.ID); got != "active" {
		t.Errorf("entity state = %q, want active — closing it would drop the replay at the closed-entity gate", got)
	}
	for _, task := range live {
		if n := closeAuditCount(t, database, task.ID); n != 0 {
			t.Errorf("close-audit rows on %s after the failed close = %d, want 0", task.ID, n)
		}
	}

	ripenQueue(t, database)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue after recovery: %v", err)
	}
	if n := activeTaskCount(t, database, entity.ID); n != 0 {
		t.Errorf("active tasks after the replay = %d, want 0", n)
	}
	if got := entityState(t, database, entity.ID); got != "closed" {
		t.Errorf("entity state = %q, want closed", got)
	}
	for _, task := range live {
		if n := closeAuditCount(t, database, task.ID); n != 1 {
			t.Errorf("close-audit rows on %s = %d, want exactly 1", task.ID, n)
		}
	}
}

// mergedQueueRow reads the terminating event's queue row. queueRow assumes a
// single row in the table; these tests enqueue the CI events that set the
// scene first, so the read is keyed on the event type instead.
func mergedQueueRow(t *testing.T, database *sql.DB) (status string, attempts int, lastErr string) {
	t.Helper()
	if err := database.QueryRow(`
		SELECT q.status, q.attempt, COALESCE(q.last_error, '')
		FROM event_queue q JOIN events e ON e.id = q.event_id
		WHERE e.event_type = ?`, domain.EventGitHubPRMerged).
		Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("read the merged queue row: %v", err)
	}
	return status, attempts, lastErr
}
