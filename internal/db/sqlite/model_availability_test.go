package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestModelAvailabilityStore_SQLite_Conformance runs the shared lifecycle
// contract against the SQLite impl. The org id is the local sentinel because
// every SQLite store asserts it — BootstrapSchemaForTest already seeds the
// matching orgs row the FK needs.
func TestModelAvailabilityStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunModelAvailabilityStoreConformance(t, func(t *testing.T) (db.ModelAvailabilityStore, string) {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.New(conn).ModelAvailability, runmode.LocalDefaultOrgID
	})
}

// TestModelAvailabilityStore_SQLite_SurvivesReopen is the durability half of
// the lifecycle: green is permanent, and "permanent" has to mean across a
// process restart or the save gate re-offers a paid test on every boot.
//
// It runs against a file on disk rather than the shared in-memory fixture,
// closing the handle entirely between the write and the read — which is the
// only shape in which an in-memory implementation of this store would fail.
func TestModelAvailabilityStore_SQLite_SurvivesReopen(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "availability.db") +
		"?_pragma=foreign_keys(on)&" + db.SQLiteTimeFormatParam

	open := func() *sql.DB {
		t.Helper()
		conn, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open %s: %v", dsn, err)
		}
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
		return conn
	}

	first := open()
	if err := db.Migrate(first, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SeedLocalTenantRows(t.Context(), first); err != nil {
		t.Fatalf("seed local tenant: %v", err)
	}
	written, err := sqlitestore.New(first).ModelAvailability.Record(
		t.Context(), runmode.LocalDefaultOrgID, "anthropic", "claude-sonnet-5", domain.ModelAvailabilityGreen, "")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := open()
	t.Cleanup(func() { _ = second.Close() })
	row, err := sqlitestore.New(second).ModelAvailability.Get(
		t.Context(), runmode.LocalDefaultOrgID, "anthropic", "claude-sonnet-5")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if row == nil {
		t.Fatal("the green row did not survive the reopen")
	}
	if row.State != domain.ModelAvailabilityGreen {
		t.Errorf("state after reopen = %q, want %q", row.State, domain.ModelAvailabilityGreen)
	}
	if !row.CheckedAt.Equal(written.CheckedAt) {
		t.Errorf("checked_at after reopen = %s, want the stamp the write returned %s", row.CheckedAt, written.CheckedAt)
	}
}
