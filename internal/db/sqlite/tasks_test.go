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
		// production seeds it via BootstrapAgentForOrg. Replicate that
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
		// Per-team multi-team conformance test creates a
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

	// List must refuse non-LocalDefaultOrgID even though the underlying
	// SQL would happily run — the guard is the only place that catches
	// a "I think I'm in multi mode" caller.
	if _, _, err := store.List(t.Context(), "some-other-org", db.TaskListFilter{}, db.ListOpts{Limit: 50}); err == nil {
		t.Error("List accepted non-LocalDefaultOrgID without error")
	}
}

// TestTaskStore_SQLite_SlackMessageCount pins the derived Slack thread
// message-count column: a Slack task's SlackMessageCount reflects the number
// of slack:message events on its entity, and the source gate keeps it zero for
// non-Slack tasks.
func TestTaskStore_SQLite_SlackMessageCount(t *testing.T) {
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
	ctx := t.Context()
	now := time.Now().UTC()

	entityID := uuid.New().String()
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
		VALUES (?, 'slack', 'C1/1600000000.000100', 'thread', 'New thread messages in #general', '', '{}', ?)
	`, entityID, now); err != nil {
		t.Fatalf("seed slack entity: %v", err)
	}
	// Three slack:message events on the thread; the first spawns the task.
	var firstEventID string
	for i := 0; i < 3; i++ {
		evID := uuid.New().String()
		if i == 0 {
			firstEventID = evID
		}
		if _, err := conn.Exec(`
			INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
			VALUES (?, ?, ?, '', '{}', ?)
		`, evID, entityID, domain.EventSlackMessage, now); err != nil {
			t.Fatalf("seed slack event %d: %v", i, err)
		}
	}
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
		                   status, priority_score, scoring_status, created_at, team_id, visibility)
		VALUES (?, ?, ?, '', ?, 'queued', 0.5, 'pending', ?, ?, 'team')
	`, taskID, entityID, domain.EventSlackMessage, firstEventID, now, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed slack task: %v", err)
	}

	got, err := store.Get(ctx, runmode.LocalDefaultOrgID, taskID)
	if err != nil {
		t.Fatalf("Get slack task: %v", err)
	}
	if got.SlackMessageCount != 3 {
		t.Errorf("SlackMessageCount = %d; want 3", got.SlackMessageCount)
	}

	// A GitHub task's count stays zero — the source gate skips the correlated
	// count entirely, even though the entity has its own (non-slack) event.
	_, _, ghTask := seedSQLiteTaskChain(t, conn, "ghcount")
	ghGot, err := store.Get(ctx, runmode.LocalDefaultOrgID, ghTask)
	if err != nil {
		t.Fatalf("Get github task: %v", err)
	}
	if ghGot.SlackMessageCount != 0 {
		t.Errorf("github SlackMessageCount = %d; want 0", ghGot.SlackMessageCount)
	}
}

// TestTaskStore_SQLite_ReassignClaimToUser covers the TFAC-561 user↔user
// handoff arm: a CAS that only lands when the task is presently claimed by
// the expected "from" user AND the target shares a team with the task, and
// refuses on a stale expectation, an unrelated target, an unclaimed/bot-
// claimed task, or a terminal row.
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
	// userB shares LocalDefaultTeamID with the seeded task chain (every
	// seedSQLiteTaskChain task's team_id) — a legitimate reassign target.
	// userC has no team membership at all — the target-relevance guard's
	// negative case.
	userB := uuid.New().String()
	userC := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO users (id, display_name) VALUES (?, 'User B'), (?, 'User C')`, userB, userC,
	); err != nil {
		t.Fatalf("seed second/third user: %v", err)
	}
	if _, err := conn.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`,
		userB, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed userB membership: %v", err)
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

	// TFAC-561 review fix: a target with zero relationship to the task's
	// team must be refused — landing the claim anyway would leave userC
	// unable to even see the row afterward (tasks_select RLS requires team
	// membership on a claimed task).
	t.Run("refuses_target_with_no_team_membership", func(t *testing.T) {
		_, _, taskID := seedSQLiteTaskChain(t, conn, "reassign-no-team")
		if ok, err := store.ClaimQueuedForUser(ctx, runmode.LocalDefaultOrgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		ok, err := store.ReassignClaimToUser(ctx, runmode.LocalDefaultOrgID, taskID, userA, userC)
		if err != nil {
			t.Fatalf("reassign to unrelated target: %v", err)
		}
		if ok {
			t.Error("ok=true reassigning to a user with no team membership; target-relevance guard broken")
		}
		got, _ := store.Get(ctx, runmode.LocalDefaultOrgID, taskID)
		if got.ClaimedByUserID != userA {
			t.Errorf("claim was disturbed by the refused reassign: got %q want %q", got.ClaimedByUserID, userA)
		}
	})

	// ReassignClaimToUserSystem is a thin same-connection wrapper in
	// SQLite (local mode has one pool) — pin that it behaves identically
	// to ReassignClaimToUser rather than silently diverging.
	t.Run("system_variant_matches_plain", func(t *testing.T) {
		_, _, taskID := seedSQLiteTaskChain(t, conn, "reassign-system")
		if ok, err := store.ClaimQueuedForUser(ctx, runmode.LocalDefaultOrgID, taskID, userA); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		ok, err := store.ReassignClaimToUserSystem(ctx, runmode.LocalDefaultOrgID, taskID, userA, userB)
		if err != nil {
			t.Fatalf("ReassignClaimToUserSystem: %v", err)
		}
		if !ok {
			t.Fatal("ReassignClaimToUserSystem returned ok=false on a valid user-claimed task")
		}
		got, _ := store.Get(ctx, runmode.LocalDefaultOrgID, taskID)
		if got.ClaimedByUserID != userB {
			t.Errorf("ClaimedByUserID=%q, want %q", got.ClaimedByUserID, userB)
		}
	})
}

// TestTaskStore_SQLite_MarkEventInjectedSystem pins TFAC-621 part 2:
// MarkEventInjectedSystem flips an EXISTING (task_id, event_id) row's kind
// to "injected" in place, and no-ops (no error, no row created) when the
// row is absent — it must never mask over RecordEventSystem's own INSERT-OR-
// IGNORE no-op the way calling RecordEventSystem(..., "injected") a second
// time on the same PK silently did.
func TestTaskStore_SQLite_MarkEventInjectedSystem(t *testing.T) {
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
	ctx := t.Context()

	readKind := func(t *testing.T, taskID, eventID string) (kind string, found bool) {
		t.Helper()
		err := conn.QueryRow(`SELECT kind FROM task_events WHERE task_id = ? AND event_id = ?`, taskID, eventID).Scan(&kind)
		if err == sql.ErrNoRows {
			return "", false
		}
		if err != nil {
			t.Fatalf("read task_events kind: %v", err)
		}
		return kind, true
	}

	t.Run("upgrades_existing_row", func(t *testing.T) {
		_, eventID, taskID := seedSQLiteTaskChain(t, conn, "mark-injected-existing")
		if err := store.RecordEvent(ctx, runmode.LocalDefaultOrgID, taskID, eventID, "bumped"); err != nil {
			t.Fatalf("seed bumped row: %v", err)
		}
		if _, err := store.MarkEventInjectedSystem(ctx, runmode.LocalDefaultOrgID, taskID, eventID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("MarkEventInjectedSystem: %v", err)
		}
		kind, found := readKind(t, taskID, eventID)
		if !found {
			t.Fatal("row disappeared after MarkEventInjectedSystem")
		}
		if kind != "injected" {
			t.Errorf("kind = %q, want %q", kind, "injected")
		}
	})

	t.Run("absent_row_is_noop", func(t *testing.T) {
		_, eventID, taskID := seedSQLiteTaskChain(t, conn, "mark-injected-absent")
		// No RecordEvent seed at all — (taskID, eventID) has no row.
		if _, err := store.MarkEventInjectedSystem(ctx, runmode.LocalDefaultOrgID, taskID, eventID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("MarkEventInjectedSystem on absent row: %v", err)
		}
		if _, found := readKind(t, taskID, eventID); found {
			t.Error("MarkEventInjectedSystem created a row on a no-op absent-row call")
		}
	})
}
