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

// TestPollReadinessStore_SQLite_LastPollTimes pins the map read the team
// activity node stitches from: completed sources appear with their stamp, a
// restart clears a source's stamp and drops it from the map, and a source
// that never polled is simply absent.
func TestPollReadinessStore_SQLite_LastPollTimes(t *testing.T) {
	conn := newSQLiteForArtifactTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const orgID = "00000000-0000-0000-0000-000000000001"

	if times, err := stores.PollReadiness.LastPollTimes(ctx, orgID); err != nil || len(times) != 0 {
		t.Fatalf("LastPollTimes before any poll = %v err=%v, want empty", times, err)
	}
	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "github", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := stores.PollReadiness.MarkPollComplete(ctx, orgID, "jira", time.Time{}); err != nil {
		t.Fatal(err)
	}
	times, err := stores.PollReadiness.LastPollTimes(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 {
		t.Fatalf("times = %v, want github + jira", times)
	}
	for src, at := range times {
		if since := time.Since(at); since < 0 || since > time.Minute {
			t.Errorf("%s last poll %v is not a recent stamp", src, at)
		}
	}
	if err := stores.PollReadiness.MarkRestarted(ctx, orgID, "jira"); err != nil {
		t.Fatal(err)
	}
	times, err = stores.PollReadiness.LastPollTimes(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := times["jira"]; ok || len(times) != 1 {
		t.Fatalf("after jira restart times = %v, want github only", times)
	}
}
