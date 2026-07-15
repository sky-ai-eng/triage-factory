package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

func TestCuratorHomeStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunCuratorHomeStoreConformance(t, func(t *testing.T) (db.CuratorHomeStore, string) {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.NewCuratorHomeStore(conn), "00000000-0000-0000-0000-000000000001"
	})
}
