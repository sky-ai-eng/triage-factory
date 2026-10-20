package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRateLimitReadingStore_SQLite_Conformance runs the shared contract
// against the SQLite impl, under the local sentinel org every SQLite store
// asserts.
func TestRateLimitReadingStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunRateLimitReadingStoreConformance(t, func(t *testing.T) (db.RateLimitReadingStore, string) {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.New(conn).RateLimitReadings, runmode.LocalDefaultOrgID
	})
}
