package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProjectStore_Postgres runs the shared conformance suite against
// the Postgres ProjectStore impl. Both pools wire against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path.
// creator_user_id under admin pool resolves via the org-owner
// fallback half of the COALESCE (no JWT claims set → tf.current_user_id()
// is NULL); production multi-mode under WithTx hits the first branch.
func TestProjectStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	dbtest.RunProjectStoreConformance(t, func(t *testing.T) (db.ProjectStore, string, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := seedPgProjectOrg(t, h)
		teamID := firstTeamForOrg(t, h, orgID)
		return stores.Projects, orgID, teamID
	})
}

// TestProjectStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + mutation path.
func TestProjectStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, _, _ := seedPgProjectOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgA)
	orgB, _, _ := seedPgProjectOrg(t, h)

	id, err := stores.Projects.Create(ctx, orgA, teamA, domain.Project{
		Name: "orgA project", Description: "secret",
	})
	if err != nil {
		t.Fatalf("Create orgA: %v", err)
	}

	if got, err := stores.Projects.Get(ctx, orgB, id); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA project %s", id)
	}

	if got, err := stores.Projects.List(ctx, orgB); err != nil {
		t.Fatalf("List cross-org: %v", err)
	} else if len(got) != 0 {
		t.Errorf("orgB List returned %d rows, want 0", len(got))
	}

	if err := stores.Projects.Update(ctx, orgB, domain.Project{ID: id, Name: "hack"}); err == nil {
		t.Errorf("orgB Update on orgA project should error")
	}
	if err := stores.Projects.Delete(ctx, orgB, id); err == nil {
		t.Errorf("orgB Delete on orgA project should error")
	}
	if got, _ := stores.Projects.Get(ctx, orgA, id); got == nil || got.Name != "orgA project" {
		t.Errorf("orgA's row was clobbered by cross-org mutation: got=%+v", got)
	}
}

// TestProjectStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for projects. Where CrossOrgLeakage above wires both pools
// against AdminDB to prove the defense-in-depth WHERE-clause filter
// is intact, this test runs the store through the app pool under
// tf_app with real JWT claims so the actual projects_select /
// projects_modify policies are exercised. Same-org reads succeed;
// cross-org reads are silently filtered (USING); cross-org Create
// raises 42501 from projects_modify WITH CHECK.
func TestProjectStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgProjectOrg(t, h)
	orgB, bob, _ := seedPgProjectOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgA)

	// Seed a project in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	projA, err := stores.Projects.Create(ctx, orgA, teamA, domain.Project{
		Name: "orgA RLS project", Description: "secret",
	})
	if err != nil {
		t.Fatalf("seed project in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Projects.Get(ctx, orgA, projA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got == nil {
				t.Errorf("alice Get(orgA, projA) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Projects.Get(ctx, orgA, projA)
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got != nil {
				t.Errorf("bob Get(orgA, projA) returned %+v; RLS USING filter leaked orgA's project to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; Create against orgA would
		// land a row with org_id=orgA. projects_modify WITH CHECK
		// requires org_id = tf.current_org_id(), so 42501 is the
		// expected outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, e := pgstore.NewForTx(tx, pgtest.SecretKey).Projects.Create(ctx, orgA, teamA, domain.Project{
				Name: "cross-org write attempt",
			})
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// TestProjectStore_Postgres_CreateRefusesTeamSentinel pins the team
// guard: passing runmode.LocalDefaultTeamID (the SQLite-only sentinel)
// returns a clear error instead of silently attaching the project to
// any team. Projects are user-driven writes; the human picks the
// team at the Create UI, and the store refuses to make one
// up. Once D9 retrofits handler claims, the caller threads a real
// team from request context.
func TestProjectStore_Postgres_CreateRefusesTeamSentinel(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgID, _, _ := seedPgProjectOrg(t, h)

	_, err := stores.Projects.Create(ctx, orgID,
		runmode.LocalDefaultTeamID,
		domain.Project{Name: "no-team"})
	if err == nil {
		t.Fatal("Create with team sentinel should error; want explicit team_id requirement")
	}
}

// TestProjectStore_Postgres_CrossTeamRLSHidesProject is the
// regression guard for the projects-panel and backfill-candidates
// handlers. Both list entities by project_id but gate the listing on
// Projects.Get returning a non-nil project first (project == nil →
// early return, no entities). Entities are org-wide (polling is
// org-wide, per CLAUDE.md's standing rule on multi-mode read scoping),
// so that gate is the *only* thing stopping a user from listing
// another team's project entities. This pins that a team-visibility
// project owned by a team the viewer doesn't belong to is invisible to
// Projects.Get under RLS — so the handler's gate holds.
func TestProjectStore_Postgres_CrossTeamRLSHidesProject(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	// One org, two teams. alice is on teamA only; bob owns teamB's project.
	orgID, alice, _ := seedPgProjectOrg(t, h)
	teamA := firstTeamForOrg(t, h, orgID)
	_ = teamA
	bob := seedPgMember(t, h, orgID, "bob", "member")
	teamB := seedPgDefaultTeam(t, h, orgID, bob)

	// Seed a team-visibility project owned by teamB, created by bob, via
	// the admin pool (bypassing RLS for the seed). alice is neither the
	// creator nor a teamB member, so projects_select must hide it from her.
	projB := uuid.New().String()
	if _, err := h.AdminDB.Exec(`
		INSERT INTO projects (id, org_id, creator_user_id, team_id, visibility, name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'team', 'TeamB Secret', now(), now())
	`, projB, orgID, bob, teamB); err != nil {
		t.Fatalf("seed teamB project: %v", err)
	}

	// alice (teamA) — the panel/backfill gate must see nil and bail.
	if err := h.WithUser(t, alice, orgID, func(tx *sql.Tx) error {
		got, e := pgstore.NewForTx(tx, pgtest.SecretKey).Projects.Get(ctx, orgID, projB)
		if e != nil {
			return e
		}
		if got != nil {
			t.Errorf("alice Get(teamB project) = %+v; RLS leaked another team's project, so the entity listing gate would open", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("alice path: %v", err)
	}

	// bob (teamB) — same-team owner still sees it, so the gate doesn't
	// over-restrict the legitimate viewer.
	if err := h.WithUser(t, bob, orgID, func(tx *sql.Tx) error {
		got, e := pgstore.NewForTx(tx, pgtest.SecretKey).Projects.Get(ctx, orgID, projB)
		if e != nil {
			return e
		}
		if got == nil {
			t.Errorf("bob Get(teamB project) = nil; RLS wrongly hid the project from its own team member")
		}
		return nil
	}); err != nil {
		t.Fatalf("bob path: %v", err)
	}
}

func seedPgProjectOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("project-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Project Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Project Org "+orgID[:8], "proj-"+orgID[:8], userID,
	); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID,
	); err != nil {
		t.Fatalf("seed org_membership: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Project Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}

// TestProjectStore_Postgres_GetPopulatesTeamID pins the plumbing the
// per-project pinned-repos / Jira-rule validation depends on: a project
// created under a specific (non-default) team must round-trip its team_id
// back through Get and List. Without this, the PATCH handler can't scope
// validation to the project's own team and falls back to the org default
// — pinning repos against the wrong team's tracked set.
func TestProjectStore_Postgres_GetPopulatesTeamID(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()

	orgA, _, _ := seedPgProjectOrg(t, h)
	// A second, non-default team in the same org.
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")

	id, err := stores.Projects.Create(ctx, orgA, teamB, domain.Project{Name: "B's project"})
	if err != nil {
		t.Fatalf("Create under teamB: %v", err)
	}

	got, err := stores.Projects.Get(ctx, orgA, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.TeamID != teamB {
		t.Fatalf("Get().TeamID = %q, want teamB %q", teamID(got), teamB)
	}

	list, err := stores.Projects.List(ctx, orgA)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, p := range list {
		if p.ID == id {
			found = true
			if p.TeamID != teamB {
				t.Errorf("List row TeamID = %q, want teamB %q", p.TeamID, teamB)
			}
		}
	}
	if !found {
		t.Errorf("List did not return the created project %s", id)
	}
}

func teamID(p *domain.Project) string {
	if p == nil {
		return "<nil>"
	}
	return p.TeamID
}
