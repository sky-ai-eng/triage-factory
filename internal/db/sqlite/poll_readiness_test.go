package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPollReadinessStore_SQLite_ReadyConformance runs the shared readiness
// contract against the SQLite impl.
func TestPollReadinessStore_SQLite_ReadyConformance(t *testing.T) {
	dbtest.RunPollReadinessReadyConformance(t, func(t *testing.T) (db.PollReadinessStore, string, dbtest.PollReadinessSeeder) {
		conn := newSQLiteForDeliverableTest(t)
		return sqlitestore.New(conn).PollReadiness, runmode.LocalDefaultOrgID, dbtest.PollReadinessSeeder{
			RestartedAt: func(t *testing.T, orgID, source string) time.Time {
				t.Helper()
				var at time.Time
				if err := conn.QueryRow(`SELECT restarted_at FROM poll_readiness WHERE org_id = ? AND source = ?`,
					orgID, source).Scan(&at); err != nil {
					t.Fatalf("read restarted_at: %v", err)
				}
				return at
			},
			SetLastPollAt: func(t *testing.T, orgID, source string, at time.Time) {
				t.Helper()
				if _, err := conn.Exec(`UPDATE poll_readiness SET last_poll_at = ? WHERE org_id = ? AND source = ?`,
					at.UTC(), orgID, source); err != nil {
					t.Fatalf("set last_poll_at: %v", err)
				}
			},
		}
	})
}

// TestPollReadinessStore_SQLite_AnnouncePendingIsOneShot pins the "at most
// once" consume contract.
func TestPollReadinessStore_SQLite_AnnouncePendingIsOneShot(t *testing.T) {
	conn := newSQLiteForDeliverableTest(t)
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
	conn := newSQLiteForDeliverableTest(t)
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

// TestPollReadinessStore_SQLite_ConnectionConformance runs the shared
// connection-status contract against the SQLite impl. The org is the local
// sentinel, whose orgs row BootstrapSchemaForTest seeds.
func TestPollReadinessStore_SQLite_ConnectionConformance(t *testing.T) {
	dbtest.RunPollReadinessConnectionConformance(t, func(t *testing.T) (db.PollReadinessStore, string, func(*testing.T, string, time.Duration)) {
		conn := newSQLiteForDeliverableTest(t)
		age := func(t *testing.T, source string, d time.Duration) {
			t.Helper()
			var changed time.Time
			if err := conn.QueryRow(`SELECT connection_changed_at FROM poll_readiness WHERE org_id = ? AND source = ?`,
				runmode.LocalDefaultOrgID, source).Scan(&changed); err != nil {
				t.Fatalf("read connection_changed_at: %v", err)
			}
			if _, err := conn.Exec(`UPDATE poll_readiness SET connection_changed_at = ? WHERE org_id = ? AND source = ?`,
				changed.Add(-d).UTC(), runmode.LocalDefaultOrgID, source); err != nil {
				t.Fatalf("age the connection state: %v", err)
			}
		}
		return sqlitestore.New(conn).PollReadiness, runmode.LocalDefaultOrgID, age
	})
}
