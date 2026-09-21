package sqlite_test

import (
	"database/sql"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// TestWorkKindRegistry_SQLite runs the registry assertions over the bundle
// the SQLite constructor builds.
func TestWorkKindRegistry_SQLite(t *testing.T) {
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	dbtest.AssertWorkKindRegistry(t, sqlitestore.New(database).WorkKinds)
}
