package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The ci_check_passed → ci_check_failed close is gated on the whole suite
// being green, and "green" has to mean COMPLETED green. The snapshot keeps
// only the latest run per check name, so a failed check being re-run is
// represented by a queued/in_progress row with no conclusion — a snapshot
// with nothing failing but something still running is a suite whose verdict
// is not in yet. These tests pin the gate on both sides of that line: a
// sibling's pass while the failed check re-runs must hold the task, and the
// re-run's own completion must release it.

// seedPRCheckRuns writes a PR snapshot holding exactly the given check runs,
// standing in for the tracker's post-refresh commit (the state runCloses
// reads when the pass event routes).
func seedPRCheckRuns(t *testing.T, database *sql.DB, entityID string, runs ...domain.CheckRun) {
	t.Helper()
	snap, err := json.Marshal(domain.PRSnapshot{CheckRuns: runs})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if _, err := sqlitestore.New(database).Entities.UpdateSnapshot(
		t.Context(), runmode.LocalDefaultOrgID, entityID, string(snap)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

// enqueueCIPassed durably enqueues a ci_check_passed event for the named
// check — the trigger side of the close relation under test.
func enqueueCIPassed(t *testing.T, database *sql.DB, entityID, checkName string) {
	t.Helper()
	eid := entityID
	meta, _ := json.Marshal(events.GitHubPRCICheckPassedMetadata{
		Author: "aidan", CheckName: checkName, Conclusion: "success",
		Repo: "owner/repo", PRNumber: 1, HeadSHA: "abc123",
	})
	if _, err := sqlitestore.New(database).EventQueue.Enqueue(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		OrgID:        runmode.LocalDefaultOrgID,
		EntityID:     &eid,
		EventType:    domain.EventGitHubPRCICheckPassed,
		MetadataJSON: string(meta),
	}, ""); err != nil {
		t.Fatalf("enqueue ci_check_passed(%s): %v", checkName, err)
	}
}

// activeCIFailedTasks reads the close relation's own target list: the active
// ci_check_failed tasks on the entity.
func activeCIFailedTasks(t *testing.T, database *sql.DB, entityID string) []domain.Task {
	t.Helper()
	tasks, err := testTaskStore(database).FindActiveByEntityAndTypeSystem(
		t.Context(), runmode.LocalDefaultOrgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil {
		t.Fatalf("list active ci_check_failed tasks: %v", err)
	}
	return tasks
}

// TestCloseCIPassed_RerunInFlightHoldsTheTask is the incident this gate arm
// exists for. One check fails and mints the task; the failed check is re-run,
// which replaces its failing snapshot row with an in-progress one; an
// unrelated check then passes. Nothing in the snapshot is failing at that
// moment, but nothing has resolved either — the close must wait for the
// re-run's own verdict, and land only when it completes green.
func TestCloseCIPassed_RerunInFlightHoldsTheTask(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#rerun-window")

	// The failed check is re-running: latest run by name has no conclusion.
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 2, Name: "compose-smoke", Status: "in_progress"},
		domain.CheckRun{ID: 3, Name: "test", Status: "completed", Conclusion: "success"},
	)
	enqueueCIPassed(t, database, entityID, "test")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the sibling's pass: %v", err)
	}
	if live := activeCIFailedTasks(t, database, entityID); len(live) != 1 {
		t.Fatalf("active ci_check_failed tasks = %d, want 1 — a sibling's pass closed the task while the failed check was still re-running", len(live))
	}

	// The re-run completes green: the suite has a verdict, and the check that
	// failed is the one whose pass carries the close.
	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 4, Name: "compose-smoke", Status: "completed", Conclusion: "success"},
		domain.CheckRun{ID: 3, Name: "test", Status: "completed", Conclusion: "success"},
	)
	enqueueCIPassed(t, database, entityID, "compose-smoke")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the recovery pass: %v", err)
	}
	if live := activeCIFailedTasks(t, database, entityID); len(live) != 0 {
		t.Fatalf("active ci_check_failed tasks = %d, want 0 — the suite is green and the task must close", len(live))
	}

	var closeEventType string
	if err := database.QueryRow(`SELECT close_event_type FROM tasks WHERE id = ?`, taskID).Scan(&closeEventType); err != nil {
		t.Fatalf("read close_event_type: %v", err)
	}
	if closeEventType != domain.EventGitHubPRCICheckPassed {
		t.Errorf("close_event_type = %q, want %q", closeEventType, domain.EventGitHubPRCICheckPassed)
	}
}

// TestCloseCIPassed_SiblingStillFailingHoldsTheTask pins the gate's other
// refusal arm: a pass while another check holds a failing conclusion does not
// close the task.
func TestCloseCIPassed_SiblingStillFailingHoldsTheTask(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)

	entityID, _ := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#still-red")

	seedPRCheckRuns(t, database, entityID,
		domain.CheckRun{ID: 1, Name: "compose-smoke", Status: "completed", Conclusion: "failure"},
		domain.CheckRun{ID: 3, Name: "test", Status: "completed", Conclusion: "success"},
	)
	enqueueCIPassed(t, database, entityID, "test")
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drain the pass: %v", err)
	}
	if live := activeCIFailedTasks(t, database, entityID); len(live) != 1 {
		t.Fatalf("active ci_check_failed tasks = %d, want 1 — a check is still failing", len(live))
	}
}
