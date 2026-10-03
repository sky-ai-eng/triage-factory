package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
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

// TestPollReadinessStore_Postgres_LastPollTimes mirrors the SQLite case: see
// that file for what each step pins.
func TestPollReadinessStore_Postgres_LastPollTimes(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	const orgID = "22222222-2222-2222-2222-222222222222"

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

// TestPollReadinessStore_Postgres_ConnectionConformance runs the shared
// connection-status contract against the Postgres impl. The store is
// admin-pool only, so there is no RLS path to wire it through.
func TestPollReadinessStore_Postgres_ConnectionConformance(t *testing.T) {
	dbtest.RunPollReadinessConnectionConformance(t, func(t *testing.T) (db.PollReadinessStore, string) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "connections")
		return pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey).PollReadiness, orgID
	})
}

// TestPollReadinessStore_Postgres_ConnectionListSkipsDeletedOrgs pins the
// scoping the SQLite suite structurally cannot: a soft-deleted org's last
// recorded state stops being reported, so a tenant that is gone never counts
// toward the deployment's connection gauge.
func TestPollReadinessStore_Postgres_ConnectionListSkipsDeletedOrgs(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	live, _, _ := pgtest.SeedOrgWithUser(t, h, "live")
	gone, _, _ := pgtest.SeedOrgWithUser(t, h, "gone")
	store := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey).PollReadiness
	ctx := context.Background()

	for _, org := range []string{live, gone} {
		if _, _, err := store.RecordConnection(ctx, org, "github", db.ConnectionDown, "transient"); err != nil {
			t.Fatalf("RecordConnection %s: %v", org, err)
		}
	}
	if _, err := h.AdminDB.ExecContext(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, gone); err != nil {
		t.Fatalf("soft-delete org: %v", err)
	}

	list, err := store.ListConnectionStatuses(ctx)
	if err != nil {
		t.Fatalf("ListConnectionStatuses: %v", err)
	}
	if len(list) != 1 || list[0].OrgID != live {
		t.Errorf("ListConnectionStatuses = %+v, want only the live org's row", list)
	}
}
