package postgres_test

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestEventHandlerStore_Postgres runs the shared conformance suite
// against the Postgres impl. AdminDB serves both pools so Seed
// (admin-only, no JWT claims at boot) and CRUD reads (app pool with
// claims) both work without per-subtest plumbing — same shape the
// other Postgres conformance tests use.
//
// The PromptSeeder inserts a system-source prompt per requested slug;
// event_handlers.prompt_id has composite FKs to prompts(id, org_id) AND
// the same-team prompts(id, team_id) (SKY-380), so trigger fixtures need
// real prompt rows owned by the factory's team, and the seeder returns the
// slug→prompt-id map the harness threads into Seed / Create.
func TestEventHandlerStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, string, dbtest.PromptSeeder) {
		t.Helper()
		h.Reset(t)
		orgID := seedPgOrgForAgents(t, h)
		// Seed requires a team. seedPgOrgForAgents already stages the org's
		// default team (via seedPgDefaultTeam); firstTeamForOrg picks it up
		// by the same created_at ordering production used to do implicitly.
		teamID := firstTeamForOrg(t, h, orgID)
		stores := pgstore.New(h.AdminDB, h.AdminDB)
		seed := func(t *testing.T, slugs ...string) map[string]string {
			t.Helper()
			out := make(map[string]string, len(slugs))
			for _, slug := range slugs {
				// system-source rows ship with creator_user_id NULL +
				// visibility='team' and a system_slug (SKY-380). The id is a
				// random UUID; seed via SeedOrUpdate (admin pool) so the row
				// + version sidecar land correctly and we capture the minted
				// id for the trigger→prompt same-team FK.
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
