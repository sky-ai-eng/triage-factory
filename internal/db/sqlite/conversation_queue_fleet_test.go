package sqlite_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestConversationQueueStore_SQLite_FleetReads exercises the fleet-dashboard + org-ops
// reads (TFAC-589): queue depth, queued ages (cross-org + org-scoped), and the
// conversation-timing projection with its terminal-conversation
// duration/claim stamps.
func TestConversationQueueStore_SQLite_FleetReads(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// A firing apiece: two conversations queued at the same instant are two
	// delegations, which is two blueprint_runs on two tasks.
	for i, id := range []string{"fr-run-0", "fr-run-1"} {
		bpID, taskID, promptID := seedSqliteFiringParents(t, conn, stores, fmt.Sprintf("fleet-reads-%d", i))
		fireSqliteStep(t, conn, stores, bpID, taskID, domain.Conversation{
			ID: id, PromptID: promptID, Model: "claude-sonnet-4-6",
		})
	}

	// Two queued conversations.
	if n, err := stores.ConversationQueue.CountQueuedSystem(ctx); err != nil || n != 2 {
		t.Fatalf("CountQueuedSystem = (%d, %v), want 2", n, err)
	}
	queued, err := stores.ConversationQueue.QueuedConversationAgesSystem(ctx)
	if err != nil || len(queued) != 2 {
		t.Fatalf("QueuedConversationAgesSystem = (%d, %v), want 2", len(queued), err)
	}
	// Org filter: this org has both; a different org has none.
	if q, _ := stores.ConversationQueue.QueuedConversationAgesForOrgSystem(ctx, org); len(q) != 2 {
		t.Fatalf("QueuedConversationAgesForOrgSystem(self) = %d, want 2", len(q))
	}
	if q, _ := stores.ConversationQueue.QueuedConversationAgesForOrgSystem(ctx, "00000000-0000-0000-0000-0000000000ff"); len(q) != 0 {
		t.Fatalf("QueuedConversationAgesForOrgSystem(other org) = %d, want 0", len(q))
	}

	// Complete one conversation with a claim so the timing read has a terminal
	// row with a queue wait and a duration. Both claim identity AND duration
	// live on the claims row now (the timing read derives claimed_at from the
	// latest claim, duration from the claims' telemetry SUM).
	if _, err := conn.Exec(`
		UPDATE conversations SET status='completed',
		       completed_at=datetime(started_at, '+7 seconds')
		WHERE id='fr-run-0'`); err != nil {
		t.Fatalf("complete conversation: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO claims (id, conversation_id, executor_id, boot_epoch, claimed_at, released_at, outcome, duration_ms)
		SELECT 'fr-claim-0', id, 'fr-exec', 1, datetime(started_at, '+2 seconds'),
		       datetime(started_at, '+7 seconds'), 'completed', 5000
		FROM conversations WHERE id='fr-run-0'`); err != nil {
		t.Fatalf("stamp claim: %v", err)
	}
	if n, _ := stores.ConversationQueue.CountQueuedSystem(ctx); n != 1 {
		t.Fatalf("CountQueuedSystem after completing one = %d, want 1", n)
	}

	timings, err := stores.ConversationQueue.RecentConversationTimingsSystem(ctx, time.Unix(0, 0), 0)
	if err != nil || len(timings) != 2 {
		t.Fatalf("RecentConversationTimingsSystem = (%d, %v), want 2", len(timings), err)
	}
	var completed *domain.ConversationTiming
	for i := range timings {
		if timings[i].Status == "completed" {
			completed = &timings[i]
		}
	}
	if completed == nil {
		t.Fatal("expected a completed conversation in the timing read")
	}
	if completed.DurationMS == nil || *completed.DurationMS != 5000 {
		t.Fatalf("completed duration = %v, want 5000", completed.DurationMS)
	}
	if completed.ClaimedAt == nil {
		t.Fatalf("completed conversation should carry claimed_at for the queue-wait metric")
	}

	// Org filter on the timing read (no until bound).
	if ts, _ := stores.ConversationQueue.RecentConversationTimingsForOrgSystem(ctx, org, time.Unix(0, 0), time.Time{}, 0); len(ts) != 2 {
		t.Fatalf("RecentConversationTimingsForOrgSystem(self) = %d, want 2", len(ts))
	}
	if ts, _ := stores.ConversationQueue.RecentConversationTimingsForOrgSystem(ctx, "00000000-0000-0000-0000-0000000000ff", time.Unix(0, 0), time.Time{}, 0); len(ts) != 0 {
		t.Fatalf("RecentConversationTimingsForOrgSystem(other org) = %d, want 0", len(ts))
	}

	// until bound is honored: an upper bound before both conversations'
	// started_at (which are ~now) excludes them; a bound in the future
	// includes both.
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if ts, _ := stores.ConversationQueue.RecentConversationTimingsForOrgSystem(ctx, org, time.Unix(0, 0), past, 0); len(ts) != 0 {
		t.Fatalf("RecentConversationTimingsForOrgSystem with until in the past = %d, want 0 (upper bound applied)", len(ts))
	}
	future := time.Now().UTC().Add(time.Hour)
	if ts, _ := stores.ConversationQueue.RecentConversationTimingsForOrgSystem(ctx, org, time.Unix(0, 0), future, 0); len(ts) != 2 {
		t.Fatalf("RecentConversationTimingsForOrgSystem with until in the future = %d, want 2", len(ts))
	}
}
