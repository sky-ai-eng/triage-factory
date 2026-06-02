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
// The BlueprintSeeder inserts a system-source blueprint per requested slug;
// event_handlers.blueprint_id has composite FKs to blueprints(id, org_id) AND
// the same-team blueprints(id, team_id), so trigger fixtures need real
// blueprint rows owned by the factory's team, and the seeder returns the
// slug→blueprint-id map the harness threads into Seed / Create.
func TestEventHandlerStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)

	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, string, dbtest.BlueprintSeeder) {
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
				// system-source rows ship with creator_user_id NULL and a
				// system_slug. The id is a random UUID; seed via SeedOrUpdate
				// (admin pool) and capture the minted id for the
				// trigger→blueprint same-team FK.
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
