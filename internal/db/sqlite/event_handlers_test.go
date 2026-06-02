package sqlite_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEventHandlerStore_SQLite runs the shared conformance suite
// against the SQLite impl. Trigger fixtures FK to blueprints(id, org_id);
// the seedBlueprints hook inserts the named rows via BlueprintStore so the
// harness stays schema-blind.
func TestEventHandlerStore_SQLite(t *testing.T) {
	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, string, dbtest.BlueprintSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		orgID := runmode.LocalDefaultOrgID
		teamID := runmode.LocalDefaultTeamID
		seed := func(t *testing.T, slugs ...string) map[string]string {
			t.Helper()
			out := make(map[string]string, len(slugs))
			for _, slug := range slugs {
				id, err := stores.Blueprints.SeedOrUpdate(t.Context(), orgID, teamID, domain.Blueprint{
					SystemSlug: slug, Name: slug, Source: "system",
				})
				if err != nil {
					t.Fatalf("seed blueprint %s: %v", slug, err)
				}
				out[slug] = id
			}
			return out
		}
		return stores.EventHandlers, orgID, teamID, seed
	})
}
