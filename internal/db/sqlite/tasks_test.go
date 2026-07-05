package sqlite_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskStore_SQLite runs the shared conformance suite against the
// SQLite TaskStore impl. The factory opens a fresh in-memory DB per
// subtest, seeds the local-default agent + user so claim FKs resolve,
// and returns a seeder that creates fresh entity+event+task chains
// for each assertion.
func TestTaskStore_SQLite(t *testing.T) {
	dbtest.RunTaskStoreConformance(t, func(t *testing.T) (db.TaskStore, string, string, string, string, dbtest.TaskSeeder, dbtest.TeamSeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)

		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}
		// The local-default agent + user sentinels are seeded by the
		// migration's defaults, but the agents row itself isn't —
		// production seeds it via BootstrapLocalAgent. Replicate that
		// shape inline so claim methods that FK into agents resolve.
		if _, err := conn.Exec(
			`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
			runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
		); err != nil {
			t.Fatalf("seed local agent: %v", err)
		}

		stores := sqlitestore.New(conn)
		seeder := func(t *testing.T, suffix string) (entityID, eventID, taskID string) {
			t.Helper()
			return seedSQLiteTaskChain(t, conn, suffix)
		}
		// SKY-295: per-team multi-team conformance test creates a
		// secondary team alongside LocalDefaultTeamID. Local mode is
		// single-team in production, but the SQLite schema doesn't
		// reject additional teams — useful for exercising the
		// per-team dedup index from one in-memory DB.
		teamSeeder := func(t *testing.T, suffix string) string {
			t.Helper()
			id := uuid.New().String()
			if _, err := conn.Exec(
				`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
				id, runmode.LocalDefaultOrgID, "team-"+suffix+"-"+id[:8], "Conformance Team "+suffix,
			); err != nil {
				t.Fatalf("seed extra team: %v", err)
			}
			return id
		}
		return stores.Tasks, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID, runmode.LocalDefaultUserID, seeder, teamSeeder
	})
}

// seedSQLiteTaskChain inserts a fresh entity + event + task chain for
// one conformance subtest. Each call uses a unique source_id so the
// dedup index doesn't collapse independent seeds across subtests.
func seedSQLiteTaskChain(t *testing.T, conn *sql.DB, suffix string) (entityID, eventID, taskID string) {
	t.Helper()
	now := time.Now().UTC()
	entityID = uuid.New().String()
	taskID = uuid.New().String()
	eventID = uuid.New().String()
	// suffix + nanos keeps source_id unique within and across subtests.
	sourceID := fmt.Sprintf("conformance-%s-%d", suffix, now.UnixNano())
	eventType := domain.EventGitHubPRCICheckFailed

	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'github', ?, 'pr', ?, ?, '{}', ?)
	`, entityID, sourceID, "Conformance "+suffix, "https://example/"+sourceID, now); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
		VALUES (?, ?, ?, '', '{}', ?)
	`, eventID, entityID, eventType, now); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at,
		                   team_id, visibility)
		VALUES (?, ?, ?, '', ?, 'queued', 0.5, 'pending', ?, ?, 'team')
	`, taskID, entityID, eventType, eventID, now, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return entityID, eventID, taskID
}

// TestTaskStore_SQLite_AssertLocalOrg covers the local-only invariant
// that's specific to the SQLite impl — the orgID guard at every
// method entry refuses anything other than LocalDefaultOrgID. The
// conformance suite already exercises the happy path; this test
// pins the SQLite-specific rejection.
func TestTaskStore_SQLite_AssertLocalOrg(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	store := sqlitestore.New(conn).Tasks

	// Queued must refuse non-LocalDefaultOrgID even though the underlying
	// SQL would happily run — the guard is the only place that catches
	// a "I think I'm in multi mode" caller.
	if _, err := store.Queued(t.Context(), "some-other-org", nil); err == nil {
		t.Error("Queued accepted non-LocalDefaultOrgID without error")
	}
}

// TestTaskStore_SQLite_ReassignClaimToUser covers the TFAC-561 user↔user
// handoff arm: a CAS that only lands when the task is presently claimed by
// the expected "from" user, moves the claim to a second seeded user, and
// refuses on a stale expectation, an unclaimed/bot-claimed task, or a
// terminal row.
func TestTaskStore_SQLite_ReassignClaimToUser(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID,
	); err != nil {
		t.Fatalf("seed local agent: %v", err)
	}
	userB := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'User B')`, userB,
	); err != nil {
		t.Fatalf("seed second user: %v", err)
	}
	store := sqlitestore.New(conn).Tasks
	ctx := t.Context()
	userA := runmode.LocalDefaultUserID

	t.Run("lands_then_refuses_stale_expectation", func(t *testing.T) {
		_, _, taskID := seedSQLiteTaskChain(t, conn, "reassign-happy")
		if ok, err := store.ClaimQueuedForUser(ctx, runmode.LocalDefaultOrgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		ok, err := store.ReassignClaimToUser(ctx, runmode.LocalDefaultOrgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign: %v", err)
		}
		if !ok {
			t.Fatal("reassign returned ok=false on a valid user-claimed task")
		}
		got, _ := store.Get(ctx, runmode.LocalDefaultOrgID, taskID)
		if got.ClaimedByUserID != userB {
			t.Errorf("ClaimedByUserID=%q, want %q", got.ClaimedByUserID, userB)
		}
		if got.ClaimedByAgentID != "" {
			t.Errorf("ClaimedByAgentID=%q, want empty after reassign", got.ClaimedByAgentID)
		}
		// The claim already moved to userB — a second reassign expecting
		// userA as the "from" claimant must now be refused (stale CAS
		// expectation), and the successful reassign above must survive.
		ok, err = store.ReassignClaimToUser(ctx, runmode.LocalDefaultOrgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("stale reassign: %v", err)
		}
		if ok {
			t.Error("reassign returned ok=true against a stale expected-claimant; CAS guard broken")
		}
		got, _ = store.Get(ctx, runmode.LocalDefaultOrgID, taskID)
		if got.ClaimedByUserID != userB {
			t.Errorf("claim was disturbed by the refused CAS: got %q want %q", got.ClaimedByUserID, userB)
		}
	})

	t.Run("refuses_unclaimed_task", func(t *testing.T) {
		_, _, taskID := seedSQLiteTaskChain(t, conn, "reassign-unclaimed")
		ok, err := store.ReassignClaimToUser(ctx, runmode.LocalDefaultOrgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign on unclaimed: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning an unclaimed task; guard broken")
		}
	})

	t.Run("refuses_bot_claimed_task", func(t *testing.T) {
		_, _, taskID := seedSQLiteTaskChain(t, conn, "reassign-bot")
		if _, err := store.StampAgentClaimIfUnclaimed(ctx, runmode.LocalDefaultOrgID, taskID, runmode.LocalDefaultAgentID, ""); err != nil {
			t.Fatalf("stamp bot claim: %v", err)
		}
		ok, err := store.ReassignClaimToUser(ctx, runmode.LocalDefaultOrgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign on bot-claimed: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning a bot-claimed task; the from-user CAS should never match a NULL claimed_by_user_id")
		}
	})

	t.Run("refuses_terminal_task", func(t *testing.T) {
		_, _, taskID := seedSQLiteTaskChain(t, conn, "reassign-term")
		if ok, err := store.ClaimQueuedForUser(ctx, runmode.LocalDefaultOrgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		if err := store.Close(ctx, runmode.LocalDefaultOrgID, taskID, "test", ""); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err := store.ReassignClaimToUser(ctx, runmode.LocalDefaultOrgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("reassign on terminal: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning a closed task; status guard broken")
		}
	})
}
