package postgres_test

import (
	"testing"

	"github.com/google/uuid"
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
		stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
		seed := func(t *testing.T, slugs ...string) map[string]string {
			t.Helper()
			out := make(map[string]string, len(slugs))
			for _, slug := range slugs {
				// The trigger→blueprint same-team FK only needs a blueprint row to
				// exist; a plain user blueprint satisfies it (the shipped system-slug
				// shape is exercised in the shipped-defaults sync suite). The id is a
				// random UUID, captured for the FK wiring.
				id := uuid.New().String()
				if err := stores.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{
					ID: id, Name: slug, Source: "user",
				}); err != nil {
					t.Fatalf("seed blueprint %s: %v", slug, err)
				}
				out[slug] = id
			}
			return out
		}
		return stores.EventHandlers, orgID, teamID, seed
	})
}
