package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestBootstrapNewOrg_Postgres_TwoPool is the SKY-385 regression guard.
//
// The bug: BootstrapTeamAgent looked up the org's agent via the app-pool
// GetForOrg. The whole bootstrap chain runs outside any WithTx with no
// JWT claims, so the app pool is a bare `authenticator` connection with
// no table grants — the lookup failed with "permission denied for table
// agents" (42501) and the team_agents bot row was never written, leaving
// the org's first team with auto-delegation silently dead.
//
// The load-bearing detail of this test is the pool wiring:
// pgstore.New(admin=AdminDB, app=AppDB). The existing SQLite test can't
// catch the bug (no pool split, no RLS), and — the easy trap — neither
// can a pg test wired pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey) like most of the
// suite: with AdminDB in the app slot the buggy app-pool lookup succeeds
// and the test is a no-op. Only the real two-pool split, with AppDB as
// the grant-less app pool, reproduces production and fails pre-fix.
func TestBootstrapNewOrg_Postgres_TwoPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	// Tenancy rows the founder-signup tx would have committed before the
	// post-commit bootstrap runs.
	orgID, _, teamID := pgtest.SeedOrgWithUser(t, h, "sky385")

	// Production wiring: admin = supabase_admin (BYPASSRLS), app =
	// authenticator (no grants outside the SET ROLE tf_app + claims tx).
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	ctx := context.Background()
	if err := db.BootstrapNewOrg(ctx, stores, orgID, teamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg on the two-pool store: %v", err)
	}

	// Read back through the admin pool — the app pool can't SELECT outside
	// a claims tx either. The team_agents bot row is the row that silently
	// went missing pre-fix.
	agent, err := stores.Agents.GetForOrgSystem(ctx, orgID)
	if err != nil {
		t.Fatalf("GetForOrgSystem: %v", err)
	}
	if agent == nil {
		t.Fatal("no agents row after BootstrapNewOrg")
	}
	ta, err := stores.TeamAgents.GetForTeamSystem(ctx, orgID, teamID, agent.ID)
	if err != nil {
		t.Fatalf("GetForTeamSystem: %v", err)
	}
	if ta == nil {
		t.Fatal("default team has no team_agents bot row after BootstrapNewOrg (SKY-385 regression)")
	}
	if !ta.Enabled {
		t.Error("team_agents.enabled=false; want true so auto-delegation can fire")
	}
}

// TestAppPoolRead_NoClaims_WrapsPermErr is the SKY-387 deliverable-D
// guard: an app-pool store read reached without the SET ROLE tf_app +
// JWT-claims ceremony (the exact shape of the SKY-385 bug — a
// system/bootstrap caller hitting an app-pool method) must surface the
// actionable, *System-naming error from wrapAppPoolPermErr, not the raw
// "permission denied for table agents" 42501.
//
// Like the two-pool regression guard above, the load-bearing detail is
// the wiring: pgstore.New(admin=AdminDB, app=AppDB) with AppDB the
// grant-less `authenticator` pool. GetForOrg goes to the app pool; with
// no claims tx the bare authenticator has no table grant and Postgres
// raises 42501 — the runtime failure mode the wrap translates.
func TestAppPoolRead_NoClaims_WrapsPermErr(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgID, _, _ := pgtest.SeedOrgWithUser(t, h, "sky387")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	_, err := stores.Agents.GetForOrg(context.Background(), orgID)
	if err == nil {
		t.Fatal("expected permission-denied error from a bare app-pool GetForOrg, got nil")
	}

	// The underlying *pgconn.PgError (42501) is preserved via %w, so
	// SQLSTATE-inspecting callers still see it.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("want wrapped SQLSTATE 42501, got %v", err)
	}

	// ...and the message is self-explaining: names the callsite and points
	// at the *System fix.
	if !strings.Contains(err.Error(), "agents.GetForOrg") {
		t.Errorf("error does not name the callsite: %v", err)
	}
	if !strings.Contains(err.Error(), "*System variant") {
		t.Errorf("error does not point at the *System fix: %v", err)
	}
}
