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

// TestProjectStore_SQLite runs the shared conformance suite against
// the SQLite ProjectStore impl. Each subtest gets a fresh in-memory DB.
func TestProjectStore_SQLite(t *testing.T) {
	dbtest.RunProjectStoreConformance(t, func(t *testing.T) (db.ProjectStore, string, string) {
		t.Helper()
		conn := newSQLiteForProjectTest(t)
		stores := sqlitestore.New(conn)
		return stores.Projects, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	})
}

// TestProjectStore_SQLite_RejectsNonLocalOrg pins the assertLocalOrg
// guard on every method.
func TestProjectStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	conn := newSQLiteForProjectTest(t)
	store := sqlitestore.New(conn).Projects
	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	ctx := t.Context()

	if _, err := store.Create(ctx, bogusOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "x"}); err == nil {
		t.Errorf("Create with non-local orgID should error")
	}
	if _, err := store.Get(ctx, bogusOrg, "any"); err == nil {
		t.Errorf("Get with non-local orgID should error")
	}
	if _, _, err := store.List(ctx, bogusOrg, db.ListOpts{Limit: 200}); err == nil {
		t.Errorf("List with non-local orgID should error")
	}
	if err := store.Update(ctx, bogusOrg, domain.Project{ID: "any", Name: "x"}); err == nil {
		t.Errorf("Update with non-local orgID should error")
	}
	if err := store.Delete(ctx, bogusOrg, "any"); err == nil {
		t.Errorf("Delete with non-local orgID should error")
	}
}

func newSQLiteForProjectTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
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
