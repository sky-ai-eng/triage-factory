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
)

// TestRepoStore_Postgres runs the shared conformance suite against
// the Postgres RepoStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path;
// the cross-org leakage test below exercises the org_id filter
// directly.
func TestRepoStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunRepoStoreConformance(t, func(t *testing.T) (db.RepoStore, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := seedPgRepoOrg(t, h)
		return stores.Repos, orgID
	})
}

// TestRepoStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + mutation path. RLS via
// repo_profiles_all also enforces this, but the org_id = $N clause
// in each query is the belt to RLS's suspenders.
func TestRepoStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, _, _ := seedPgRepoOrg(t, h)
	orgB, _, _ := seedPgRepoOrg(t, h)
	ctx := context.Background()

	// Seed a repo into orgA only.
	if err := stores.Repos.Upsert(ctx, orgA, domain.RepoProfile{
		ID: "octo/widget", Owner: "octo", Repo: "widget",
		Description: "orgA widget", ProfileText: "orgA body",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("Upsert orgA: %v", err)
	}

	// Get(orgB, octo/widget) must return nil despite the row existing.
	if got, err := stores.Repos.Get(ctx, orgB, "octo/widget"); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA repo %s", got.ID)
	}

	// List cross-org must return empty.
	got, err := stores.Repos.List(ctx, orgB)
	if err != nil {
		t.Fatalf("List cross-org: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("orgB List returned %d rows, want 0", len(got))
	}

	// ListWithContent cross-org must also return empty.
	gotContent, err := stores.Repos.ListWithContent(ctx, orgB)
	if err != nil {
		t.Fatalf("ListWithContent cross-org: %v", err)
	}
	if len(gotContent) != 0 {
		t.Errorf("orgB ListWithContent returned %d rows, want 0", len(gotContent))
	}

	// CountConfigured cross-org must report 0.
	if n, _ := stores.Repos.CountConfigured(ctx, orgB); n != 0 {
		t.Errorf("orgB CountConfigured = %d, want 0", n)
	}

	// UpdateBaseBranch cross-org must not touch orgA's row.
	if err := stores.Repos.UpdateBaseBranch(ctx, orgB, "octo/widget", "hack"); err != nil {
		t.Fatalf("UpdateBaseBranch cross-org: %v", err)
	}
	if got, _ := stores.Repos.Get(ctx, orgA, "octo/widget"); got.BaseBranch != "" {
		t.Errorf("orgA's BaseBranch was mutated by orgB UpdateBaseBranch: got %q", got.BaseBranch)
	}

	// UpdateCloneStatus cross-org must not touch orgA's row.
	if err := stores.Repos.UpdateCloneStatus(ctx, orgB, "octo", "widget", "failed", "hack", "other"); err != nil {
		t.Fatalf("UpdateCloneStatus cross-org: %v", err)
	}
	if got, _ := stores.Repos.Get(ctx, orgA, "octo/widget"); got.CloneStatus == "failed" {
		t.Errorf("orgA's CloneStatus was mutated by orgB UpdateCloneStatus: got %q", got.CloneStatus)
	}

	// SetConfigured cross-org must not delete orgA's row.
	if err := stores.Repos.SetConfigured(ctx, orgB, []string{"another/repo"}); err != nil {
		t.Fatalf("SetConfigured cross-org: %v", err)
	}
	if got, _ := stores.Repos.Get(ctx, orgA, "octo/widget"); got == nil {
		t.Errorf("orgA's repo was deleted by orgB SetConfigured")
	}
}

// TestRepoStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for repo_profiles. Where CrossOrgLeakage above wires both
// pools against AdminDB to prove the defense-in-depth WHERE-clause
// filter is intact, this test runs the store through the app pool
// under tf_app with real JWT claims so the actual repo_profiles_all
// policy is exercised. Same-org reads succeed; cross-org reads are
// silently filtered (USING); cross-org Upsert raises 42501 from the
// WITH CHECK side of the same policy.
func TestRepoStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgRepoOrg(t, h)
	orgB, bob, _ := seedPgRepoOrg(t, h)
	_ = alice
	_ = bob

	// Seed a repo in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	if err := stores.Repos.UpsertSystem(ctx, orgA, domain.RepoProfile{
		ID: "octo/rls", Owner: "octo", Repo: "rls",
		Description: "orgA rls repo", ProfileText: "body",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("seed repo in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Repos.Get(ctx, orgA, "octo/rls")
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got == nil {
				t.Errorf("alice Get(orgA, octo/rls) returned nil; same-org RLS USING filter wrongly excluded the row")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("alice path: %v", err)
		}
	})

	t.Run("cross_org_read_filtered", func(t *testing.T) {
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Repos.Get(ctx, orgA, "octo/rls")
			if err != nil {
				return fmt.Errorf("Get: %w", err)
			}
			if got != nil {
				t.Errorf("bob Get(orgA, octo/rls) returned %+v; RLS USING filter leaked orgA's repo to orgB", got)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("bob read path: %v", err)
		}
	})

	t.Run("cross_org_write_denied", func(t *testing.T) {
		// bob's claims point at orgB; Upsert against orgA would land
		// a row with org_id=orgA. The repo_profiles_all policy's
		// WITH CHECK requires org_id = tf.current_org_id(), so 42501
		// is the expected outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			return pgstore.NewForTx(tx, pgtest.SecretKey).Repos.Upsert(ctx, orgA, domain.RepoProfile{
				ID: "octo/rls-write", Owner: "octo", Repo: "rls-write",
				Description: "x", ProfileText: "x", DefaultBranch: "main",
			})
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

func seedPgRepoOrg(t *testing.T, h *pgtest.Harness) (orgID, userID, agentID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	agentID = uuid.New().String()
	email := fmt.Sprintf("repo-%s@test.local", userID[:8])

	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`,
		userID, "Repo Conformance User",
	); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	if _, err := h.AdminDB.Exec(
		`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Repo Org "+orgID[:8], "repo-"+orgID[:8], userID,
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
		`INSERT INTO agents (id, org_id, display_name) VALUES ($1, $2, 'Repo Bot')`,
		agentID, orgID,
	); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	return orgID, userID, agentID
}
