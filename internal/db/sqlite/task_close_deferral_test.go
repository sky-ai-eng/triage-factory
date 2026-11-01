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
	dbtest.RunTaskCloseDeferralConformance(t, func(t *testing.T) (dbtest.TaskCloseDeferralStores, string, dbtest.TaskCloseDeferralSeeder) {
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
			DraftPullRequest: func(t *testing.T, conversationID string) {
				exec(t, "seed draft pull request", `
					INSERT INTO deliverables (id, conversation_id, org_id, team_id, provider, kind, scope, target, state, dedup_key)
					SELECT ?, id, org_id, team_id, 'github', 'pull_request', 'github.com', 'acme/widget#7', 'draft', 'held-close-' || id
					  FROM conversations WHERE id = ?
				`, uuid.New().String(), conversationID)
			},
		}
		return dbtest.TaskCloseDeferralStores{
			Tasks:         stores.Tasks,
			Entities:      stores.Entities,
			Conversations: stores.Conversations,
		}, runmode.LocalDefaultOrgID, seed
	})
}
