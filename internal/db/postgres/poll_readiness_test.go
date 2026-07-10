package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestPollReadinessStore_Postgres_ReadyLifecycle pins the org-scoped
// readiness gate's core contract: not ready until a poll completes, a
// restart clears readiness until a fresh completion lands, and a stale
// (pre-restart) completion is ignored.
func TestPollReadinessStore_Postgres_ReadyLifecycle(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const orgID = "22222222-2222-2222-2222-222222222222"

	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || ready {
		t.Fatalf("Ready before any poll: ready=%v err=%v, want false/nil", ready, err)
	}

	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "jira", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || !ready {
		t.Fatalf("Ready after first completion: ready=%v err=%v, want true/nil", ready, err)
	}

	restartedAt := time.Now()
	if err := stores.PollReadiness.MarkRestarted(ctx, orgID, "jira"); err != nil {
		t.Fatal(err)
	}
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || ready {
		t.Fatalf("Ready right after restart: ready=%v err=%v, want false/nil", ready, err)
	}

	// A completion whose startedAt precedes the restart is a straggler
	// from before it — must not flip readiness back to true.
	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "jira", restartedAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || ready {
		t.Fatalf("Ready after stale pre-restart completion: ready=%v err=%v, want false/nil", ready, err)
	}

	// A completion whose startedAt is after the restart is the real thing.
	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "jira", restartedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || !ready {
		t.Fatalf("Ready after fresh post-restart completion: ready=%v err=%v, want true/nil", ready, err)
	}

	// github is a distinct source — unaffected by jira's rows.
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "github"); err != nil || ready {
		t.Fatalf("Ready for a different source: ready=%v err=%v, want false/nil", ready, err)
	}
}

// TestPollReadinessStore_Postgres_AnnouncePendingIsAtomicOneShot pins the
// "at most once" consume contract: TakeAnnouncePending only reports true
// once per SetAnnouncePending, even under concurrent takers.
func TestPollReadinessStore_Postgres_AnnouncePendingIsAtomicOneShot(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const orgID = "33333333-3333-3333-3333-333333333333"

	if taken, err := stores.PollReadiness.TakeAnnouncePending(ctx, orgID, "github"); err != nil || taken {
		t.Fatalf("Take before Set: taken=%v err=%v, want false/nil", taken, err)
	}

	if err := stores.PollReadiness.SetAnnouncePending(ctx, orgID, "github"); err != nil {
		t.Fatal(err)
	}

	results := make(chan bool, 4)
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			taken, err := stores.PollReadiness.TakeAnnouncePending(ctx, orgID, "github")
			if err != nil {
				t.Error(err)
			}
			results <- taken
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	close(results)
	trueCount := 0
	for r := range results {
		if r {
			trueCount++
		}
	}
	if trueCount != 1 {
		t.Fatalf("exactly one concurrent taker should see true, got %d", trueCount)
	}
}
