package db

import "testing"

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

	if got := queryString(t, database, `SELECT added_by || ' ' || COALESCE(user_id, 'pending') FROM operators WHERE email = 'ops@example.com'`); got != "root pending" {
		t.Errorf("operator = %q, want its provenance kept and no principal", got)
	}
	if n := queryInt(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'poller_state'`); n != 0 {
		t.Error("poller_state still exists after the migration")
	}
}
