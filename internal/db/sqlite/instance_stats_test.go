package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

func TestInstanceStatStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunInstanceStatStoreConformance(t, func(t *testing.T) db.InstanceStatStore {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.NewInstanceStatStore(conn)
	})
}
