package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunPendingInputStore_SQLite runs the shared conformance suite against
// the SQLite RunPendingInputStore impl. Each subtest opens a fresh
// in-memory DB so state doesn't leak between assertions.
func TestRunPendingInputStore_SQLite(t *testing.T) {
	dbtest.RunRunPendingInputStoreConformance(t, func(t *testing.T) (db.RunPendingInputStore, string, string, dbtest.RunPendingInputSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		seed := dbtest.RunPendingInputSeeder{
			Run: func(t *testing.T, suffix string) string {
				t.Helper()
				return seedSQLiteRunForPendingInput(t, conn, suffix)
			},
			DeleteRun: func(t *testing.T, runID string) {
				t.Helper()
				if _, err := conn.Exec(`DELETE FROM conversations WHERE id = ?`, runID); err != nil {
					t.Fatalf("delete run: %v", err)
				}
			},
			SecondUser: func(t *testing.T) string {
				t.Helper()
				id := uuid.New().String()
				if _, err := conn.Exec(`INSERT INTO users (id, display_name) VALUES (?, 'teammate')`, id); err != nil {
					t.Fatalf("seed second user: %v", err)
				}
				return id
			},
		}
		return stores.RunPendingInput, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, seed
	})
}

// TestRunPendingInputStore_SQLite_RejectsNonLocalOrg pins assertLocalOrg.
func TestRunPendingInputStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const badOrg = "11111111-1111-1111-1111-111111111111"

	if err := stores.RunPendingInput.Store(ctx, badOrg, "r", "u", "msg"); err == nil {
		t.Error("Store(non-local org) should error")
	}
	if _, _, _, err := stores.RunPendingInput.Consume(ctx, badOrg, "r"); err == nil {
		t.Error("Consume(non-local org) should error")
	}
}

// seedSQLiteRunForPendingInput inserts a bare run row (origin='interactive',
// so no blueprint_run FK chain is required) the run_pending_input FK needs.
func seedSQLiteRunForPendingInput(t *testing.T, conn *sql.DB, suffix string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := conn.Exec(
		`INSERT INTO conversations (id, origin, status) VALUES (?, 'interactive', 'running')`, id,
	); err != nil {
		t.Fatalf("seed run %s (%s): %v", id, fmt.Sprintf("pending-input-%s", suffix), err)
	}
	return id
}
