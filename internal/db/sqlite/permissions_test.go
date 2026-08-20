package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPermissionStore_SQLite_Conformance runs the shared suite against the
// SQLite impl. This is the arm production actually exercises: the browser
// permission round-trip is reached only by an unsandboxed local SDK run.
func TestPermissionStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunPermissionStoreConformance(t, func(t *testing.T) (db.PermissionStore, string, string, dbtest.PermissionSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		userID := seedSQLiteUserForPermissions(t, conn)
		seed := dbtest.PermissionSeeder{
			Conversation: func(t *testing.T) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.Exec(
					`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
				); err != nil {
					t.Fatalf("seed conversation: %v", err)
				}
				return id
			},
			Claim: func(t *testing.T, conversationID string) string {
				t.Helper()
				return dbtest.SeedActiveClaim(t, conn, conversationID, "exec-perms", 0)
			},
			ReleaseClaim: func(t *testing.T, claimID string) {
				t.Helper()
				if _, err := conn.Exec(
					`UPDATE claims SET released_at = CURRENT_TIMESTAMP, outcome = 'reaped' WHERE id = ?`, claimID,
				); err != nil {
					t.Fatalf("release claim: %v", err)
				}
			},
			Load: func(t *testing.T, conversationID, toolCallID string) (dbtest.PermissionRow, bool) {
				t.Helper()
				return loadSQLitePermissionRow(t, conn, conversationID, toolCallID)
			},
		}
		return stores.Permissions, runmode.LocalDefaultOrgID, userID, seed
	})
}

// seedSQLiteUserForPermissions inserts the users row decided_by references.
func seedSQLiteUserForPermissions(t *testing.T, conn *sql.DB) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'perm-approver')`, id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// loadSQLitePermissionRow reads the audit columns straight out of the table.
// The store exposes no history read (the audit UI that would call it doesn't
// exist yet), so the suite checks them through raw SQL rather than not at all.
func loadSQLitePermissionRow(t *testing.T, conn *sql.DB, conversationID, toolCallID string) (dbtest.PermissionRow, bool) {
	t.Helper()
	var row dbtest.PermissionRow
	var reason, decidedBy sql.NullString
	var decidedAt sql.NullTime
	var waited sql.NullInt64
	err := conn.QueryRow(`
		SELECT state, reason, decided_by, decided_at, waited_ms
		FROM conversation_permissions
		WHERE conversation_id = ? AND tool_call_id = ?
	`, conversationID, toolCallID).Scan(&row.State, &reason, &decidedBy, &decidedAt, &waited)
	if err == sql.ErrNoRows {
		return dbtest.PermissionRow{}, false
	}
	if err != nil {
		t.Fatalf("load permission row: %v", err)
	}
	row.Reason = reason.String
	row.DecidedBy = decidedBy.String
	row.HasDecidedAt = decidedAt.Valid
	if waited.Valid {
		v := int(waited.Int64)
		row.WaitedMs = &v
	}
	return row, true
}
