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
// against the SQLite impl. Trigger fixtures FK to prompts(id, org_id);
// the seedPrompts hook inserts the named rows via PromptStore so the
// harness stays schema-blind.
func TestEventHandlerStore_SQLite(t *testing.T) {
	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, string, dbtest.PromptSeeder) {
		t.Helper()
		conn := openSQLiteForTest(t)
		stores := sqlitestore.New(conn)
		orgID := runmode.LocalDefaultOrgID
		teamID := runmode.LocalDefaultTeamID
		// Closure captures orgID rather than referring to
		// runmode.LocalDefaultOrgID directly — same shape as the
		// Postgres factory (which gets a per-test UUID) and keeps the
		// harness wiring consistent: whatever org the store is exercised
		// against is the org prompts get seeded into.
		seed := func(t *testing.T, slugs ...string) map[string]string {
			t.Helper()
			out := make(map[string]string, len(slugs))
			for _, slug := range slugs {
				id, err := stores.Prompts.SeedOrUpdate(t.Context(), orgID, teamID, domain.Prompt{
					SystemSlug: slug, Name: slug, Body: "test body", Source: "system",
				})
				if err != nil {
					t.Fatalf("seed prompt %s: %v", slug, err)
				}
				out[slug] = id
			}
			return out
		}
		return stores.EventHandlers, orgID, teamID, seed
	})
}
