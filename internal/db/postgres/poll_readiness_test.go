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

// TestPollReadinessStore_Postgres_ReadyConformance runs the shared readiness
// contract against the Postgres impl.
func TestPollReadinessStore_Postgres_ReadyConformance(t *testing.T) {
	dbtest.RunPollReadinessReadyConformance(t, func(t *testing.T) (db.PollReadinessStore, string, dbtest.PollReadinessSeeder) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "readiness")
		return pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey).PollReadiness, orgID, dbtest.PollReadinessSeeder{
			RestartedAt: func(t *testing.T, orgID, source string) time.Time {
				t.Helper()
				var at time.Time
				if err := h.AdminDB.QueryRow(`SELECT restarted_at FROM poll_readiness WHERE org_id = $1 AND source = $2`,
					orgID, source).Scan(&at); err != nil {
					t.Fatalf("read restarted_at: %v", err)
				}
				return at
			},
			SetLastPollAt: func(t *testing.T, orgID, source string, at time.Time) {
				t.Helper()
				if _, err := h.AdminDB.Exec(`UPDATE poll_readiness SET last_poll_at = $3 WHERE org_id = $1 AND source = $2`,
					orgID, source, at); err != nil {
					t.Fatalf("set last_poll_at: %v", err)
				}
			},
		}
	})
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
	dbtest.RunPollReadinessConnectionConformance(t, func(t *testing.T) (db.PollReadinessStore, string, func(*testing.T, string, time.Duration)) {
		h := pgtest.Shared(t)
		h.Reset(t)
		orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "connections")
		age := func(t *testing.T, source string, d time.Duration) {
			t.Helper()
			dbtest.MustUpdate(t, h.AdminDB, `
				UPDATE poll_readiness SET connection_changed_at = connection_changed_at - $3 * interval '1 microsecond'
				WHERE org_id = $1 AND source = $2`, orgID, source, d.Microseconds())
		}
		return pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey).PollReadiness, orgID, age
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
