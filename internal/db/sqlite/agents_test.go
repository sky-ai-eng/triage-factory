package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAgentStore_SQLite runs the shared AgentStore conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB so
// row state doesn't leak across cases.
func TestAgentStore_SQLite(t *testing.T) {
	dbtest.RunAgentStoreConformance(t, func(t *testing.T) (db.AgentStore, string) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		return stores.Agents, runmode.LocalDefaultOrgID
	})
}

// TestAgentStore_SQLite_NonLocalOrgReturnsEmpty pins the current
// shape: GetForOrg with a non-local orgID returns (nil, nil) — same
// as the Postgres impl's WHERE org_id = ? semantics for a missing
// org. Previously this method called assertLocalOrg and returned an
// error; the new structural approach replaces the convention with a
// column filter that just finds no rows.
func TestAgentStore_SQLite_NonLocalOrgReturnsEmpty(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	got, err := stores.Agents.GetForOrg(t.Context(), "some-other-uuid")
	if err != nil {
		t.Fatalf("GetForOrg with non-local orgID returned error: %v", err)
	}
	if got != nil {
		t.Errorf("GetForOrg with non-local orgID returned %+v; want nil", got)
	}
}
