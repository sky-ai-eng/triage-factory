package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestTeamsStore_Postgres_GetDefaultForOrgSystem_ReturnsFirstTeam pins
// the admin-pool lookup contract: returns the oldest teams row for the
// org. Handlers that previously hardcoded runmode.LocalDefaultTeamID
// now route through this store; the test FK-binds against real
// public.teams rows so the post-fix Postgres path is exercised end-to-
// end.
func TestTeamsStore_Postgres_GetDefaultForOrgSystem_ReturnsFirstTeam(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID := seedPgOrgForAgents(t, h)
	wantTeamID := firstTeamForOrg(t, h, orgID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != wantTeamID {
		t.Errorf("GetDefaultForOrgSystem = %q; want %q (first teams row for org %s)", got, wantTeamID, orgID)
	}
}

// TestTeamsStore_Postgres_GetDefaultForOrgSystem_NoTeamsReturnsEmpty —
// an org with zero teams returns ("", nil). Handlers map the empty
// string to a 500 rather than mint a row pointing at a non-existent
// team_id. This branch exercises the FK-fail path we're trying to
// avoid by failing closed instead.
func TestTeamsStore_Postgres_GetDefaultForOrgSystem_NoTeamsReturnsEmpty(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	// Seed an org WITHOUT a default team. SeedOrg leaves the org
	// teamless on purpose for this branch — seedPgOrgForAgents would
	// add the default via seedPgDefaultTeam.
	ownerID := pgtest.SeedUser(t, h, "teamless-owner-"+uuid.New().String()[:8])
	orgID := pgtest.SeedOrg(t, h, "teamless-"+uuid.New().String()[:8], ownerID)

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	got, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem: %v", err)
	}
	if got != "" {
		t.Errorf("GetDefaultForOrgSystem on teamless org = %q; want empty string", got)
	}
}

// TestTeamsStore_Postgres_GetDefaultForOrgSystem_IsolatesPerOrg —
// the lookup is org-scoped; two orgs with their own default teams
// each resolve to their own row. This is the multi-tenancy guarantee
// the handlers depend on (factory-drop / project-create for tenant A
// must never resolve to tenant B's team).
func TestTeamsStore_Postgres_GetDefaultForOrgSystem_IsolatesPerOrg(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA := seedPgOrgForAgents(t, h)
	orgB := seedPgOrgForAgents(t, h)
	teamA := firstTeamForOrg(t, h, orgA)
	teamB := firstTeamForOrg(t, h, orgB)
	if teamA == teamB {
		t.Fatalf("test seed produced the same team for two orgs (%s)", teamA)
	}

	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	gotA, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), orgA)
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem orgA: %v", err)
	}
	gotB, err := stores.Teams.GetDefaultForOrgSystem(context.Background(), orgB)
	if err != nil {
		t.Fatalf("GetDefaultForOrgSystem orgB: %v", err)
	}
	if gotA != teamA {
		t.Errorf("orgA: GetDefaultForOrgSystem = %q; want %q", gotA, teamA)
	}
	if gotB != teamB {
		t.Errorf("orgB: GetDefaultForOrgSystem = %q; want %q", gotB, teamB)
	}
	if gotA == gotB {
		t.Errorf("default team resolved identically across two orgs: %q (per-tenant isolation broken)", gotA)
	}
}
