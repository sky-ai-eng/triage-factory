package sqlite_test

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestCuratorStore_SQLite_Conformance runs the shared curator suite over
// the conversations/messages/claims tables. SQLite has no RLS — the
// claims-bound calls are behavioral only — and no auth concept, so the
// harness identity is the local sentinel pair.
func TestCuratorStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunCuratorStoreConformance(t, func(t *testing.T) dbtest.CuratorHarness {
		conn := newSQLiteForCuratorTest(t)
		stores := sqlitestore.New(conn)
		return dbtest.CuratorHarness{
			Stores: stores,
			OrgID:  runmode.LocalDefaultOrgID,
			UserID: runmode.LocalDefaultUserID,
			SeedProject: func(t *testing.T, name string) string {
				t.Helper()
				created, err := stores.Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
					domain.Project{Name: name})
				if err != nil {
					t.Fatalf("create project %q: %v", name, err)
				}
				id := created.ID
				return id
			},
		}
	})
}

func newSQLiteForCuratorTest(t *testing.T) *sql.DB {
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
	return conn
}
