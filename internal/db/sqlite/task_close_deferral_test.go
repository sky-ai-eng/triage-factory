package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskCloseDeferral_SQLite runs the shared held-close suite against the
// SQLite impl, a fresh in-memory DB per subtest.
func TestTaskCloseDeferral_SQLite(t *testing.T) {
	dbtest.RunTaskCloseDeferralConformance(t, newSQLiteCloseDeferralFactory())
}

// TestTaskReopen_SQLite runs the shared reopen suite against the SQLite impl,
// on the held-close suite's stores and seeder.
func TestTaskReopen_SQLite(t *testing.T) {
	dbtest.RunTaskReopenConformance(t, newSQLiteCloseDeferralFactory())
}

// TestSiblingWork_SQLite runs the shared overlapping-work suite against the
// SQLite impl, on the held-close suite's stores and seeder.
func TestSiblingWork_SQLite(t *testing.T) {
	mk := newSQLiteCloseDeferral()
	dbtest.RunSiblingWorkConformance(t, func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.SiblingWorkSeeder) {
		t.Helper()
		conn, stores, orgID, seed := mk(t)
		return stores, orgID, dbtest.SiblingWorkSeeder{
			TaskCloseDeferralSeeder: seed,
			Touch: func(t *testing.T, conversationID, entityID string) {
				t.Helper()
				if _, err := conn.Exec(`
					INSERT INTO conversation_memory_entities (org_id, conversation_id, entity_id, role)
					VALUES (?, ?, ?, 'touched')
				`, orgID, conversationID, entityID); err != nil {
					t.Fatalf("seed touch: %v", err)
				}
			},
			MoveToOtherTeam: func(t *testing.T, taskID string) {
				t.Helper()
				teamID := uuid.New().String()
				if _, err := conn.Exec(`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
					teamID, orgID, "other-"+teamID[:8], "Other"); err != nil {
					t.Fatalf("seed team: %v", err)
				}
				if _, err := conn.Exec(`INSERT INTO task_teams (task_id, team_id) SELECT id, team_id FROM tasks WHERE id = ?`, taskID); err != nil {
					t.Fatalf("seed the visibility row the move leaves behind: %v", err)
				}
				if _, err := conn.Exec(`UPDATE tasks SET team_id = ? WHERE id = ?`, teamID, taskID); err != nil {
					t.Fatalf("move task: %v", err)
				}
			},
		}
	})
}

// newSQLiteCloseDeferralFactory hands each call a fresh in-memory DB with the
// held-close suite's stores and seeder over it.
func newSQLiteCloseDeferralFactory() dbtest.TaskCloseDeferralFactory {
	mk := newSQLiteCloseDeferral()
	return func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
		t.Helper()
		_, stores, orgID, seed := mk(t)
		return stores, orgID, seed
	}
}

// newSQLiteCloseDeferral is newSQLiteCloseDeferralFactory with the connection
// handed back as well, for a suite that seeds rows of its own beside the
// seeder's.
func newSQLiteCloseDeferral() func(t *testing.T) (*sql.DB, dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
	return func(t *testing.T) (*sql.DB, dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
		t.Helper()
		conn, err := sql.Open("sqlite", db.TestDSNMemory)
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		if err := db.BootstrapSchemaForTest(conn); err != nil {
			t.Fatalf("bootstrap schema: %v", err)
		}
		stores := sqlitestore.New(conn)
		exec := func(t *testing.T, what, query string, args ...any) {
			t.Helper()
			if _, err := conn.Exec(query, args...); err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		}
		seed := dbtest.TaskCloseDeferralSeeder{
			EntityTerminalCloseSeeder: newSQLiteTerminalCloseSeeder(conn),
			Claim: func(t *testing.T, conversationID string) {
				exec(t, "seed claim", `
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch, lease_expires_at)
					VALUES (?, ?, ?, 'close-hold', 1, strftime('%Y-%m-%d %H:%M:%f', 'now', '+300 seconds'))
				`, uuid.New().String(), runmode.LocalDefaultOrgID, conversationID)
			},
			HoldUntilLater: func(t *testing.T, conversationID string) {
				exec(t, "hold until later", `
					UPDATE conversations SET next_attempt_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '+1 hour') WHERE id = ?
				`, conversationID)
			},
			CallOff: func(t *testing.T, blueprintRunID string) {
				exec(t, "call off", `UPDATE blueprint_runs SET cancel_requested = 1 WHERE id = ?`, blueprintRunID)
			},
			DraftPullRequest: func(t *testing.T, conversationID, entityID string) {
				var entity any
				if entityID != "" {
					entity = entityID
				}
				exec(t, "seed draft pull request", `
					INSERT INTO deliverables (id, conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key, entity_id)
					SELECT ?, id, org_id, team_id, 'github', 'pull_request', 'github.com', 'acme/widget#7', 'draft', 'held-close-' || id, ?
					  FROM conversations WHERE id = ?
				`, uuid.New().String(), entity, conversationID)
			},
			ResolveDeliverables: func(t *testing.T, conversationID string) {
				exec(t, "resolve drafts", `UPDATE deliverables SET state = 'open' WHERE conversation_id = ? AND state = 'draft'`, conversationID)
			},
		}
		return conn, dbtest.TaskCloseDeferralStores{
			Tasks:            stores.Tasks,
			Entities:         stores.Entities,
			StagedInjections: stores.StagedInjections,
			Conversations:    stores.Conversations,
			UserID:           runmode.LocalDefaultUserID,
		}, runmode.LocalDefaultOrgID, seed
	}
}
