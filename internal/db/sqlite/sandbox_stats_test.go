package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
)

// The SQLite arm records nothing in production — local mode never sandboxes, so
// no jail exists to sample. The suite pushes real values through it to exercise
// the mechanism and hold the dual-dialect contract, not to claim a local run
// had a cgroup.
func TestSandboxStatStore_SQLite_Conformance(t *testing.T) {
	dbtest.RunSandboxStatStoreConformance(t, func(t *testing.T) db.SandboxStatStore {
		conn := newSQLiteForArtifactTest(t)
		return sqlitestore.NewSandboxStatStore(conn)
	})
}
