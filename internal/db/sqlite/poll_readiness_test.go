package sqlite_test

import (
	"context"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestPollReadinessStore_SQLite_ReadyLifecycle mirrors the Postgres
// pgtest twin: not ready until a poll completes, a restart clears
// readiness until a fresh completion lands, and a stale (pre-restart)
// completion is ignored.
func TestPollReadinessStore_SQLite_ReadyLifecycle(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const orgID = "00000000-0000-0000-0000-000000000001"

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

	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "jira", restartedAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || ready {
		t.Fatalf("Ready after stale pre-restart completion: ready=%v err=%v, want false/nil", ready, err)
	}

	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "jira", restartedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ready, err := stores.PollReadiness.Ready(ctx, orgID, "jira"); err != nil || !ready {
		t.Fatalf("Ready after fresh post-restart completion: ready=%v err=%v, want true/nil", ready, err)
	}
}

// TestPollReadinessStore_SQLite_AnnouncePendingIsOneShot pins the "at most
// once" consume contract.
func TestPollReadinessStore_SQLite_AnnouncePendingIsOneShot(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const orgID = "00000000-0000-0000-0000-000000000001"

	if taken, err := stores.PollReadiness.TakeAnnouncePending(ctx, orgID, "github"); err != nil || taken {
		t.Fatalf("Take before Set: taken=%v err=%v, want false/nil", taken, err)
	}
	if err := stores.PollReadiness.SetAnnouncePending(ctx, orgID, "github"); err != nil {
		t.Fatal(err)
	}
	if taken, err := stores.PollReadiness.TakeAnnouncePending(ctx, orgID, "github"); err != nil || !taken {
		t.Fatalf("first Take after Set: taken=%v err=%v, want true/nil", taken, err)
	}
	if taken, err := stores.PollReadiness.TakeAnnouncePending(ctx, orgID, "github"); err != nil || taken {
		t.Fatalf("second Take after Set: taken=%v err=%v, want false/nil", taken, err)
	}
}
