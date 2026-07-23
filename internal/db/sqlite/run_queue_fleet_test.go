package sqlite_test

import (
	"context"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunQueueStore_SQLite_FleetReads exercises the fleet-dashboard + org-ops
// reads (TFAC-589): queue depth, queued ages (cross-org + org-scoped), and the
// run-timing projection with its terminal-run duration/claim stamps.
func TestRunQueueStore_SQLite_FleetReads(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	task := seedEntityEventTask(t, conn, "fleet-reads")
	insertPromptForBlueprintTest(t, conn, domain.Prompt{ID: "fr-p0", Name: "Step 0", Body: "b", Source: "user"})
	insertBlueprintForTest(t, conn, "fr-bp", "FR Blueprint")
	if err := stores.Blueprints.ReplaceSteps(ctx, org, "fr-bp", []string{"fr-p0"}, nil); err != nil {
		t.Fatalf("ReplaceSteps: %v", err)
	}
	brID, err := stores.Blueprints.CreateRun(ctx, org, domain.BlueprintRun{
		ID: "fr-br", BlueprintID: "fr-bp", TaskID: task.ID,
		TriggerType: domain.BlueprintTriggerManual, Status: domain.BlueprintRunStatusRunning,
		WorktreePath: "/tmp/wt-fr",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	step0 := 0
	for _, id := range []string{"fr-run-0", "fr-run-1"} {
		if err := stores.RunQueue.EnqueueRun(ctx, org, domain.AgentRun{
			ID: id, TaskID: task.ID, PromptID: "fr-p0", Model: "claude-sonnet-4-6",
			TriggerType: "manual", BlueprintRunID: brID, BlueprintStepIndex: &step0,
		}); err != nil {
			t.Fatalf("EnqueueRun %s: %v", id, err)
		}
	}

	// Two queued runs.
	if n, err := stores.RunQueue.CountQueuedSystem(ctx); err != nil || n != 2 {
		t.Fatalf("CountQueuedSystem = (%d, %v), want 2", n, err)
	}
	queued, err := stores.RunQueue.QueuedRunAgesSystem(ctx)
	if err != nil || len(queued) != 2 {
		t.Fatalf("QueuedRunAgesSystem = (%d, %v), want 2", len(queued), err)
	}
	// Org filter: this org has both; a different org has none.
	if q, _ := stores.RunQueue.QueuedRunAgesForOrgSystem(ctx, org); len(q) != 2 {
		t.Fatalf("QueuedRunAgesForOrgSystem(self) = %d, want 2", len(q))
	}
	if q, _ := stores.RunQueue.QueuedRunAgesForOrgSystem(ctx, "00000000-0000-0000-0000-0000000000ff"); len(q) != 0 {
		t.Fatalf("QueuedRunAgesForOrgSystem(other org) = %d, want 0", len(q))
	}

	// Complete one run with a claim + duration stamp so the timing read has a
	// terminal row with a queue wait and a duration. The claim stamp is a
	// claims row now (the timing read derives claimed_at from the latest
	// claim).
	if _, err := conn.Exec(`
		UPDATE conversations SET status='completed',
		       completed_at=datetime(started_at, '+7 seconds'), duration_ms=5000
		WHERE id='fr-run-0'`); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id, boot_epoch, claimed_at, released_at, outcome)
		SELECT 'fr-claim-0', id, 'fr-exec', 1, datetime(started_at, '+2 seconds'),
		       datetime(started_at, '+7 seconds'), 'completed'
		FROM conversations WHERE id='fr-run-0'`); err != nil {
		t.Fatalf("stamp claim: %v", err)
	}
	if n, _ := stores.RunQueue.CountQueuedSystem(ctx); n != 1 {
		t.Fatalf("CountQueuedSystem after completing one = %d, want 1", n)
	}

	timings, err := stores.RunQueue.RecentRunTimingsSystem(ctx, time.Unix(0, 0), 0)
	if err != nil || len(timings) != 2 {
		t.Fatalf("RecentRunTimingsSystem = (%d, %v), want 2", len(timings), err)
	}
	var completed *domain.RunTiming
	for i := range timings {
		if timings[i].Status == "completed" {
			completed = &timings[i]
		}
	}
	if completed == nil {
		t.Fatal("expected a completed run in the timing read")
	}
	if completed.DurationMS == nil || *completed.DurationMS != 5000 {
		t.Fatalf("completed duration = %v, want 5000", completed.DurationMS)
	}
	if completed.ClaimedAt == nil {
		t.Fatalf("completed run should carry claimed_at for the queue-wait metric")
	}

	// Org filter on the timing read (no until bound).
	if ts, _ := stores.RunQueue.RecentRunTimingsForOrgSystem(ctx, org, time.Unix(0, 0), time.Time{}, 0); len(ts) != 2 {
		t.Fatalf("RecentRunTimingsForOrgSystem(self) = %d, want 2", len(ts))
	}
	if ts, _ := stores.RunQueue.RecentRunTimingsForOrgSystem(ctx, "00000000-0000-0000-0000-0000000000ff", time.Unix(0, 0), time.Time{}, 0); len(ts) != 0 {
		t.Fatalf("RecentRunTimingsForOrgSystem(other org) = %d, want 0", len(ts))
	}

	// until bound is honored: an upper bound before both runs' started_at
	// (which are ~now) excludes them; a bound in the future includes both.
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if ts, _ := stores.RunQueue.RecentRunTimingsForOrgSystem(ctx, org, time.Unix(0, 0), past, 0); len(ts) != 0 {
		t.Fatalf("RecentRunTimingsForOrgSystem with until in the past = %d, want 0 (upper bound applied)", len(ts))
	}
	future := time.Now().UTC().Add(time.Hour)
	if ts, _ := stores.RunQueue.RecentRunTimingsForOrgSystem(ctx, org, time.Unix(0, 0), future, 0); len(ts) != 2 {
		t.Fatalf("RecentRunTimingsForOrgSystem with until in the future = %d, want 2", len(ts))
	}
}
