package postgres_test

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestPermissionStore_Postgres_Conformance runs the shared suite against the
// Postgres impl. Nothing writes here in a real multi deployment — multi mints
// runtime='native', which has no permission gate, and sandboxed SDK runs
// auto-approve — so this exists to hold the dual-dialect contract, the same
// reason the SQLite sandbox_stats arm does. Skips cleanly when Docker isn't
// available (pgtest.Shared).
func TestPermissionStore_Postgres_Conformance(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunPermissionStoreConformance(t, func(t *testing.T) (db.PermissionStore, string, string, dbtest.PermissionSeeder) {
		t.Helper()
		h.Reset(t)
		orgID, userID, teamID := pgtest.SeedOrgWithUser(t, h, "alice")
		// Both halves on AdminDB: the suite drives writes (system-side in
		// production) and the pending read in one factory, and RLS isn't what's
		// under test here.
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		seed := dbtest.PermissionSeeder{
			Conversation: func(t *testing.T) string {
				t.Helper()
				return seedPgArtifactConversation(t, h, orgID, teamID, userID)
			},
			Claim: func(t *testing.T, conversationID string) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := h.AdminDB.Exec(`
					INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch)
					VALUES ($1, $2, $3, 'exec-perms', 0)
				`, id, orgID, conversationID); err != nil {
					t.Fatalf("seed claim: %v", err)
				}
				return id
			},
			ReleaseClaim: func(t *testing.T, claimID string) {
				t.Helper()
				if _, err := h.AdminDB.Exec(
					`UPDATE claims SET released_at = now(), outcome = 'reaped' WHERE id = $1`, claimID,
				); err != nil {
					t.Fatalf("release claim: %v", err)
				}
			},
			Load: func(t *testing.T, conversationID, toolCallID string) (dbtest.PermissionRow, bool) {
				t.Helper()
				return loadPgPermissionRow(t, h, conversationID, toolCallID)
			},
		}
		return stores.Permissions, orgID, userID, seed
	})
}

// loadPgPermissionRow reads the audit columns straight out of the table. The
// store exposes no history read (the audit UI that would call it doesn't exist
// yet), so the suite checks them through raw SQL rather than not at all.
func loadPgPermissionRow(t *testing.T, h *pgtest.Harness, conversationID, toolCallID string) (dbtest.PermissionRow, bool) {
	t.Helper()
	var row dbtest.PermissionRow
	var reason, decidedBy sql.NullString
	var decidedAt sql.NullTime
	var waited sql.NullInt64
	err := h.AdminDB.QueryRow(`
		SELECT state, reason, decided_by::text, decided_at, waited_ms
		FROM conversation_permissions
		WHERE conversation_id = $1 AND tool_call_id = $2
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
