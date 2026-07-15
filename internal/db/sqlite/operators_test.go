package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

func TestOperatorStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunOperatorStoreConformance(t, func(t *testing.T) db.OperatorStore {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.NewOperatorStore(conn)
	})
}
