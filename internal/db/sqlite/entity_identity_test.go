package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEntityKey_SQLite runs the key half of the entity identity model against
// the SQLite EntityStore, a fresh database per subtest.
func TestEntityKey_SQLite(t *testing.T) {
	dbtest.RunEntityKeyConformance(t, func(t *testing.T) (db.EntityStore, string) {
		t.Helper()
		return sqlitestore.New(newSQLiteForEntityTest(t)).Entities, runmode.LocalDefaultOrgID
	})
}

// TestEntityIdentity_SQLite runs the identity writes — the id lookup, the id
// stamp and the rename — against the SQLite backend.
func TestEntityIdentity_SQLite(t *testing.T) {
	dbtest.RunEntityIdentityConformance(t, func(t *testing.T) (db.Stores, string, dbtest.EntityIdentitySeeder) {
		t.Helper()
		conn := newSQLiteForEntityTest(t)
		seed := dbtest.EntityIdentitySeeder{
			TeamID: runmode.LocalDefaultTeamID,
			Task: func(t *testing.T, entityID, suffix string) string {
				t.Helper()
				return seedSQLiteTaskForRename(t, conn, entityID, suffix)
			},
			RawActionURL: func(t *testing.T, dedupKey string) (string, string) {
				t.Helper()
				var u, cur string
				if err := conn.QueryRow(
					`SELECT COALESCE(url, ''), COALESCE(current_url, '') FROM external_actions WHERE dedup_key = ?`,
					dedupKey,
				).Scan(&u, &cur); err != nil {
					t.Fatalf("read action row %q: %v", dedupKey, err)
				}
				return u, cur
			},
			// The database is this subtest's alone, so the index stays dropped.
			ForceExternalID: func(t *testing.T, entityID, externalID string) {
				t.Helper()
				if _, err := conn.Exec(`DROP INDEX entities_identity`); err != nil {
					t.Fatalf("drop identity index: %v", err)
				}
				if _, err := conn.Exec(`UPDATE entities SET external_id = ? WHERE id = ?`, externalID, entityID); err != nil {
					t.Fatalf("force external id: %v", err)
				}
			},
		}
		return sqlitestore.New(conn), runmode.LocalDefaultOrgID, seed
	})
}
