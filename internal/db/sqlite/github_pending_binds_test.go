package sqlite_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestGitHubPendingBinds_SQLite runs the shared pending-bind conformance suite
// against the SQLite impl. Each subtest opens a fresh in-memory DB.
//
// The org is the local sentinel, ensured rather than created (BootstrapSchemaForTest
// runs the same ensure the binary does) because org_id carries a real foreign
// key here. The user id is a bare uuid: user_id is a soft reference with no FK
// — the callback only ever compares it against the returning session's subject.
//
// Nothing in a local install writes this table, since the bind ceremony needs a
// deployment App and a local binary ships none. The suite runs here anyway so
// the atomic-consume guarantee is proven by the same assertions on both
// backends.
func TestGitHubPendingBinds_SQLite(t *testing.T) {
	dbtest.RunGitHubPendingBindConformance(t, func(t *testing.T) dbtest.GitHubPendingBindBackend {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)

		orgID := runmode.LocalDefaultOrgID
		if _, err := conn.Exec(
			`INSERT OR IGNORE INTO orgs (id, slug, name) VALUES (?, 'local', 'Local')`, orgID,
		); err != nil {
			t.Fatalf("seed org: %v", err)
		}

		return dbtest.GitHubPendingBindBackend{
			Store:  stores.GitHubPendingBinds,
			OrgID:  orgID,
			UserID: uuid.NewString(),
		}
	})
}
