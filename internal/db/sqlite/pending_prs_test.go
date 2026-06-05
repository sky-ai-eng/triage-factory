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

// anyPendingPR is a minimal fixture for orgID-guard tests where the
// row's actual fields don't matter — assertLocalOrg fires before the
// INSERT runs.
func anyPendingPR() domain.PendingPR {
	return domain.PendingPR{
		ID: uuid.New().String(), RunID: uuid.New().String(),
		Owner: "o", Repo: "r",
		HeadBranch: "h", HeadSHA: "s", BaseBranch: "main",
		Title: "T",
	}
}

// TestPendingPRStore_SQLite runs the shared conformance suite against
// the SQLite PendingPRStore impl. Each subtest gets a fresh in-memory
// DB.
func TestPendingPRStore_SQLite(t *testing.T) {
	dbtest.RunPendingPRStoreConformance(t, func(t *testing.T) (db.PendingPRStore, string, dbtest.PendingPRSeeder) {
		t.Helper()
		conn := newSQLiteForPendingPRTest(t)
		seed := newSQLitePendingPRSeeder(conn)
		stores := sqlitestore.New(conn)
		return stores.PendingPRs, runmode.LocalDefaultOrgID, seed
	})
}

// TestPendingPRStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard — every method must refuse a non-local orgID.
func TestPendingPRStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForPendingPRTest(t)
	stores := sqlitestore.New(conn)

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if err := stores.PendingPRs.Create(t.Context(), bogusOrg, anyPendingPR()); err == nil {
		t.Errorf("Create with non-local orgID should error")
	}
	if _, err := stores.PendingPRs.Get(t.Context(), bogusOrg, "any"); err == nil {
		t.Errorf("Get with non-local orgID should error")
	}
}

func newSQLiteForPendingPRTest(t *testing.T) *sql.DB {
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
	// Seed the prompt the seeder's run rows point at — same shape the
	// AgentRunStore SQLite test uses, separate id so the two suites
	// don't race on bootstrap.
	if _, err := conn.Exec(
		`INSERT INTO prompts (id, name, body, creator_user_id, team_id) VALUES ('p_pending_pr_test', 'Test', 'body', ?, ?)`,
		runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID,
	); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	return conn
}

// newSQLitePendingPRSeeder builds raw-SQL helpers for the conformance
// suite. SQLite enforces the pending_prs.run_id → runs(id) FK with
// ON DELETE CASCADE, so every test needs a real entity → event →
// task → run chain backing the run id we hand back.
func newSQLitePendingPRSeeder(conn *sql.DB) dbtest.PendingPRSeeder {
	return dbtest.PendingPRSeeder{
		Run: func(t *testing.T) string {
			t.Helper()
			entityID := uuid.New().String()
			eventID := uuid.New().String()
			taskID := uuid.New().String()
			runID := uuid.New().String()
			sourceID := fmt.Sprintf("pending-pr-%s", entityID[:8])

			if _, err := conn.Exec(`
				INSERT INTO entities (id, source, source_id, kind, title, url, snapshot_json, created_at)
				VALUES (?, 'github', ?, 'pr', 'PendingPR Test', ?, '{}', ?)
			`, entityID, sourceID, "https://example/"+sourceID, time.Now().UTC()); err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO events (id, entity_id, event_type, dedup_key, metadata_json, created_at)
				VALUES (?, ?, 'github:pr:opened', '', '{}', ?)
			`, eventID, entityID, time.Now().UTC()); err != nil {
				t.Fatalf("seed event: %v", err)
			}
			if _, err := conn.Exec(`
				INSERT INTO tasks (id, entity_id, event_type, dedup_key, primary_event_id,
				                   status, priority_score, scoring_status, created_at,
				                   team_id, visibility)
				VALUES (?, ?, 'github:pr:opened', '', ?, 'queued', 0.5, 'pending', ?, ?, 'team')
			`, taskID, entityID, eventID, time.Now().UTC(), runmode.LocalDefaultTeamID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			blueprintRunID := seedBlueprintRunForRun(t, conn, taskID)
			if _, err := conn.Exec(`
				INSERT INTO runs (id, task_id, prompt_id, status, model, trigger_type, team_id, blueprint_run_id)
				VALUES (?, ?, 'p_pending_pr_test', 'running', 'm', 'manual', ?, ?)
			`, runID, taskID, runmode.LocalDefaultTeamID, blueprintRunID); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			return runID
		},
	}
}
