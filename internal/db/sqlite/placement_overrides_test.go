package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

func TestPlacementOverrideStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunPlacementOverrideStoreConformance(t, func(t *testing.T) (db.PlacementOverrideStore, string) {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.NewPlacementOverrideStore(conn), "00000000-0000-0000-0000-000000000001"
	})
}
