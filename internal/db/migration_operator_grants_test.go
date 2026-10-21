package db

import (
	"database/sql"
	"testing"
)

// An operator row written before the migration keeps its email and
// provenance and comes out pending: local mode has no login identities to
// bind it to, and the column it gains is the principal a grant is bound to.
// The migration also drops poller_state, which nothing reads or writes.
func TestMigrate_OperatorGrantsGainAPrincipalAndPollerStateIsDropped(t *testing.T) {
	database := openMigrationsTestDBAt(t, TestDSNMemory, beforeProviderScopes)
	mustExec(t, database, `INSERT INTO operators (email, added_by) VALUES ('ops@example.com', 'root')`)
	mustExec(t, database, `INSERT INTO poller_state (source, source_id, state_json) VALUES ('github', 'octo/api#1', '{}')`)

	if err := Migrate(database, "sqlite3"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var addedBy string
	var userID sql.NullString
	if err := database.QueryRow(
		`SELECT added_by, user_id FROM operators WHERE email = 'ops@example.com'`,
	).Scan(&addedBy, &userID); err != nil {
		t.Fatalf("read operator: %v", err)
	}
	if addedBy != "root" || userID.Valid {
		t.Errorf("operator = (added_by %q, user_id %v), want its provenance kept and no principal", addedBy, userID)
	}

	if n := queryInt(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'poller_state'`); n != 0 {
		t.Error("poller_state still exists after the migration")
	}
}
