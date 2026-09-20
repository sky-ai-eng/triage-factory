package postgres_test

import (
	"database/sql"
	"errors"
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

	dbtest.RunEventHandlerStoreConformance(t, func(t *testing.T) (db.EventHandlerStore, string, string, dbtest.BlueprintSeeder, dbtest.ShippedHandlerIDBySlug) {
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
				if _, err := stores.Blueprints.Create(t.Context(), orgID, teamID, domain.Blueprint{
					ID: id, Name: slug, Source: "user",
				}); err != nil {
					t.Fatalf("seed blueprint %s: %v", slug, err)
				}
				out[slug] = id
			}
			return out
		}
		return stores.EventHandlers, orgID, teamID, seed, shippedHandlerIDBySlug(h, orgID)
	})
}

// shippedHandlerIDBySlug is the Postgres dbtest.ShippedHandlerIDBySlug: a raw
// admin-pool read of event_handlers by (org_id, team_id, system_slug), live
// rows only.
func shippedHandlerIDBySlug(h *pgtest.Harness, orgID string) dbtest.ShippedHandlerIDBySlug {
	return func(t *testing.T, teamID, slug string) string {
		t.Helper()
		var id string
		err := h.AdminDB.QueryRowContext(t.Context(), `
			SELECT id FROM event_handlers
			WHERE org_id = $1::uuid AND team_id = $2::uuid AND system_slug = $3 AND deleted_at IS NULL
		`, orgID, teamID, slug).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatalf("resolve shipped handler %q: %v", slug, err)
		}
		return id
	}
}
