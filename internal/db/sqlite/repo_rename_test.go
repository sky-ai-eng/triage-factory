package sqlite_test

import (
	"database/sql"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRepoRename_SQLite runs the shared rename conformance suite against the
// SQLite backend. Each subtest gets a fresh in-memory DB so the rewrite's
// all-tables-or-none assertions read a state only that subtest produced.
func TestRepoRename_SQLite(t *testing.T) {
	dbtest.RunRepoRenameConformance(t, func(t *testing.T) (db.Stores, string, dbtest.RepoRenameSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.RepoRenameSeeder{
			TeamID: runmode.LocalDefaultTeamID,
			Conversation: func(t *testing.T, suffix string) string {
				t.Helper()
				id := uuid.New().String()
				dbtest.SeedConversation(t, conn, domain.Conversation{ID: id, Model: "m"})
				return id
			},
			Task: func(t *testing.T, entityID, suffix string) string {
				t.Helper()
				return seedSQLiteTaskForRename(t, conn, entityID, suffix)
			},
			CountTasks: func(t *testing.T) int {
				t.Helper()
				var n int
				if err := conn.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&n); err != nil {
					t.Fatalf("count tasks: %v", err)
				}
				return n
			},
			ReferenceRows: func(t *testing.T) []string {
				t.Helper()
				return sqliteCategoryAReferences(t, conn)
			},
			RawActionURL: func(t *testing.T, dedupKey string) (string, string) {
				t.Helper()
				var u, cur string
				if err := conn.QueryRow(
					`SELECT COALESCE(url, ''), COALESCE(current_url, '') FROM external_actions WHERE dedup_key = ?`,
					dedupKey,
				).Scan(&u, &cur); err != nil {
					t.Fatalf("read action row %q: %v", dedupKey, err)
				}
				return u, cur
			},
		}
		return stores, runmode.LocalDefaultOrgID, seed
	})
}

// seedSQLiteTaskForRename attaches a task to an existing entity, minting the
// event row the task's FK chain requires.
func seedSQLiteTaskForRename(t *testing.T, conn *sql.DB, entityID, suffix string) string {
	t.Helper()
	const eventType = "github:pr:opened"
	eventID := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO events (id, entity_id, event_type, dedup_key) VALUES (?, ?, ?, ?)`,
		eventID, entityID, eventType, suffix,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}
	taskID := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO tasks (id, entity_id, event_type, primary_event_id, status)
		VALUES (?, ?, ?, ?, 'queued')
	`, taskID, entityID, eventType, eventID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return taskID
}

// sqliteCategoryAReferences reads the raw stored repository reference out of
// each table that carries one, so the rename suite can assert none of them
// moved. Raw SQL on purpose: a store read would resolve the id back to a slug,
// which is exactly the layer that would hide a rewrite.
func sqliteCategoryAReferences(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	queries := []struct{ label, query string }{
		{"team_github_repos", `SELECT team_id || '|' || repository_id FROM team_github_repos`},
		{"conversation_worktrees", `SELECT conversation_id || '|' || repository_id || '|' || ref FROM conversation_worktrees`},
	}
	var out []string
	for _, q := range queries {
		rows, err := conn.Query(q.query)
		if err != nil {
			t.Fatalf("read %s references: %v", q.label, err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				t.Fatalf("scan %s reference: %v", q.label, err)
			}
			out = append(out, q.label+":"+row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("read %s references: %v", q.label, err)
		}
		rows.Close()
	}
	sort.Strings(out)
	return out
}
