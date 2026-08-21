package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestOrgEventSourceStore_SQLite_Conformance runs the shared contract against
// the SQLite impl. The org id is the local sentinel because every SQLite store
// asserts it — BootstrapSchemaForTest already seeds the matching orgs row the
// FK needs.
func TestOrgEventSourceStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunOrgEventSourceStoreConformance(t, func(t *testing.T) (db.OrgEventSourceStore, string) {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.New(conn).OrgEventSources, runmode.LocalDefaultOrgID
	})
}
