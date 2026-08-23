package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRepositoryStore_Postgres runs the shared conformance suite against
// the Postgres RepositoryStore impl. Wires both pools against AdminDB
// (BYPASSRLS) so behavior tests stay independent of the auth path;
// the cross-org leakage test below exercises the org_id filter
// directly.
func TestRepositoryStore_Postgres(t *testing.T) {
	h := pgtest.Shared(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	dbtest.RunRepositoryStoreConformance(t, func(t *testing.T) (db.RepositoryStore, string) {
		t.Helper()
		h.Reset(t)
		orgID, _, _ := seedPgRepoOrg(t, h)
		return stores.Repos, orgID
	})
}

// TestRepositoryStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org_id filter on every read + mutation path. RLS via
// repositories_all also enforces this, but the org_id = $N clause
// in each query is the belt to RLS's suspenders.
func TestRepositoryStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)

	orgA, _, _ := seedPgRepoOrg(t, h)
	orgB, _, _ := seedPgRepoOrg(t, h)
	ctx := context.Background()

	// Seed a repo into orgA only.
	if _, err := stores.Repos.Upsert(ctx, orgA, domain.Repository{
		Owner: "octo", Repo: "widget",
		Description: "orgA widget", ProfileText: "orgA body",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("Upsert orgA: %v", err)
	}

	// Get(orgB, octo/widget) must return nil despite the row existing.
	if got, err := stores.Repos.GetByRef(ctx, orgB, domain.RepoRefFromSlug("octo/widget")); err != nil {
		t.Fatalf("Get cross-org: %v", err)
	} else if got != nil {
		t.Errorf("orgB Get returned orgA repo %s", got.ID)
	}

	// List cross-org must return empty.
	got, total, err := stores.Repos.List(ctx, orgB, db.ListOpts{Limit: 50})
	if err != nil {
		t.Fatalf("List cross-org: %v", err)
	}
	if len(got) != 0 || total != 0 {
		t.Errorf("orgB List returned %d rows (total %d), want 0 / 0", len(got), total)
	}

	// CountConfigured cross-org must report 0.
	if n, _ := stores.Repos.CountConfigured(ctx, orgB); n != 0 {
		t.Errorf("orgB CountConfigured = %d, want 0", n)
	}

	// UpdateBaseBranch cross-org must not touch orgA's row. The id-keyed
	// writer is handed orgA's REAL row id under orgB's org scope, which is the
	// only shape of this attack that survives the split: a caller who has
	// somehow learned the handle still cannot reach across the tenant, and
	// gets told the row does not exist rather than silently writing nothing.
	rowA, err := stores.Repos.GetByRef(ctx, orgA, domain.RepoRefFromSlug("octo/widget"))
	if err != nil || rowA == nil {
		t.Fatalf("read orgA's row: got=%v err=%v", rowA, err)
	}
	if _, err := stores.Repos.UpdateBaseBranch(ctx, orgB, rowA.ID, "hack"); !errors.Is(err, db.ErrNoSuchRepository) {
		t.Errorf("cross-org UpdateBaseBranch = %v, want db.ErrNoSuchRepository", err)
	}
	if got, _ := stores.Repos.GetByRef(ctx, orgA, domain.RepoRefFromSlug("octo/widget")); got.BaseBranch != "" {
		t.Errorf("orgA's BaseBranch was mutated by orgB UpdateBaseBranch: got %q", got.BaseBranch)
	}

	// UpdateCloneStatus cross-org must not touch orgA's row.
	if _, err := stores.Repos.UpdateCloneStatusByRef(ctx, orgB, domain.RepoRef{Owner: "octo", Repo: "widget"}, "failed", "hack", "other"); err != nil {
		t.Fatalf("UpdateCloneStatus cross-org: %v", err)
	}
	if got, _ := stores.Repos.GetByRef(ctx, orgA, domain.RepoRefFromSlug("octo/widget")); got.CloneStatus == "failed" {
		t.Errorf("orgA's CloneStatus was mutated by orgB UpdateCloneStatus: got %q", got.CloneStatus)
	}

	// SetConfigured cross-org must not delete orgA's row.
	if err := stores.Repos.SetConfigured(ctx, orgB, []string{"another/repo"}); err != nil {
		t.Fatalf("SetConfigured cross-org: %v", err)
	}
	if got, _ := stores.Repos.GetByRef(ctx, orgA, domain.RepoRefFromSlug("octo/widget")); got == nil {
		t.Errorf("orgA's repo was deleted by orgB SetConfigured")
	}
}

// TestRepositoryStore_Postgres_CrossOrgRLSDenied pins the production RLS
// layer for repositories. Where CrossOrgLeakage above wires both
// pools against AdminDB to prove the defense-in-depth WHERE-clause
// filter is intact, this test runs the store through the app pool
// under tf_app with real JWT claims so the actual repositories_all
// policy is exercised. Same-org reads succeed; cross-org reads are
// silently filtered (USING); cross-org Upsert raises 42501 from the
// WITH CHECK side of the same policy.
func TestRepositoryStore_Postgres_CrossOrgRLSDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, alice, _ := seedPgRepoOrg(t, h)
	orgB, bob, _ := seedPgRepoOrg(t, h)
	_ = alice
	_ = bob

	// Seed a repo in orgA via admin so the row exists.
	stores := pgstore.New(h.AdminDB, h.AdminDB, pgtest.SecretKey)
	ctx := context.Background()
	if _, err := stores.Repos.UpsertSystem(ctx, orgA, domain.Repository{
		Owner: "octo", Repo: "rls",
		Description: "orgA rls repo", ProfileText: "body",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("seed repo in orgA: %v", err)
	}

	t.Run("same_org_user_can_read", func(t *testing.T) {
		err := h.WithUser(t, alice, orgA, func(tx *sql.Tx) error {
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Repos.GetByRef(ctx, orgA, domain.RepoRefFromSlug("octo/rls"))
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
			got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Repos.GetByRef(ctx, orgA, domain.RepoRefFromSlug("octo/rls"))
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
		// a row with org_id=orgA. The repositories_all policy's
		// WITH CHECK requires org_id = tf.current_org_id(), so 42501
		// is the expected outcome.
		err := h.WithUser(t, bob, orgB, func(tx *sql.Tx) error {
			_, e := pgstore.NewForTx(tx, pgtest.SecretKey).Repos.Upsert(ctx, orgA, domain.Repository{
				Owner: "octo", Repo: "rls-write",
				Description: "x", ProfileText: "x", DefaultBranch: "main",
			})
			return e
		})
		pgtest.AssertRLSViolation(t, err)
	})
}

// TestRepositoryStore_Postgres_ListTeamScoped_RLS pins the multi-mode discovery
// read (TFAC-559): under the app pool with real RLS, a member's
// ListTeamScoped semi-joins through team_github_repos and returns only the
// repos their own team(s) track — not the org-wide union. A teamless
// member gets zero rows even though the repositories table has entries.
func TestRepositoryStore_Postgres_ListTeamScoped_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	bob := pgtest.SeedUser(t, h, "bob")
	// bob is teamB's *team* admin (needed so his own ReplaceForTeam call below
	// satisfies team_github_repos_insert's tf.user_is_team_admin check) but an
	// org-level plain "member" — the role the org-admin bypass gate we're
	// testing actually reads. Team role and org role are orthogonal here.
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "admin")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.MustExec(t, h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, carol, orgA)

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	// teamA tracks acme/api; teamB tracks acme/web.
	if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, teamA, []domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}})
	}); err != nil {
		t.Fatalf("track acme/api for teamA: %v", err)
	}
	if err := stores.Tx.WithTx(ctx, orgA, bob, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, teamB, []domain.TeamGitHubRepo{{Owner: "acme", Repo: "web"}})
	}); err != nil {
		t.Fatalf("track acme/web for teamB: %v", err)
	}

	listAs := func(userID string) []string {
		t.Helper()
		var got []domain.Repository
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, _, e = tx.Repos.ListTeamScoped(ctx, orgA, db.ListOpts{Limit: 50})
			return e
		}); err != nil {
			t.Fatalf("ListTeamScoped(%s): %v", userID, err)
		}
		return repoIDs(got)
	}

	if got, want := listAs(alice), []string{"acme/api"}; !pgStrsEqual(got, want) {
		t.Errorf("alice (teamA) ListTeamScoped = %v, want %v", got, want)
	}
	if got, want := listAs(bob), []string{"acme/web"}; !pgStrsEqual(got, want) {
		t.Errorf("bob (teamB) ListTeamScoped = %v, want %v", got, want)
	}
	if got := listAs(carol); len(got) != 0 {
		t.Errorf("teamless carol ListTeamScoped = %v, want empty", got)
	}
}

// TestRepositoryStore_Postgres_ReturnedRow_AppPool is where the returned-row
// standard's RLS caveat is actually tested. The shared conformance run above
// wires both pools against AdminDB (BYPASSRLS), so it pins the SQL and not the
// policies; this one runs the same assertion through tf_app with RLS active.
//
// The load-bearing case is an upsert's UPDATE arm: `UPDATE … RETURNING` yields
// a row only if the policies let the writer SELECT it. repositories_all is
// org-scoped for USING and WITH CHECK alike, so it does today — and a policy
// narrowed later would fail here rather than silently return nothing to a
// caller that is about to publish it.
func TestRepositoryStore_Postgres_ReturnedRow_AppPool(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()
	orgID, userID, _ := seedPgRepoOrg(t, h)
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

	write := func(what string, fn func(tx db.TxStores) error) {
		t.Helper()
		if err := stores.Tx.WithTx(ctx, orgID, userID, fn); err != nil {
			t.Fatalf("%s under the app pool: %v", what, err)
		}
	}
	// The follow-up read runs through the app pool too, so both halves of the
	// comparison are what an RLS-scoped caller can see.
	read := func(id string) func() (*domain.Repository, error) {
		return func() (*domain.Repository, error) {
			var row *domain.Repository
			err := stores.Tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
				var e error
				row, e = tx.Repos.Get(ctx, orgID, id)
				return e
			})
			return row, err
		}
	}

	// ProfiledAt is set so the comparisons follow the row's one pointer field:
	// a timestamptz decoded differently on the write's RETURNING than on the
	// read's SELECT is exactly the drift this test is here to catch, and a nil
	// pointer on both sides would hide it.
	profiled := time.Now().UTC().Truncate(time.Second)
	var created, updated, branched domain.Repository
	write("Upsert (insert arm)", func(tx db.TxStores) error {
		var e error
		created, e = tx.Repos.Upsert(ctx, orgID, domain.Repository{
			Owner: "Acme", Repo: "Api",
			ProfileText: "v1", DefaultBranch: "main", ExternalID: "1296269",
			ProfiledAt: &profiled,
		})
		return e
	})
	dbtest.AssertWriteReturnedStoredRow(t, "Upsert (insert arm, app pool)", created, read(created.ID))

	write("Upsert (update arm)", func(tx db.TxStores) error {
		var e error
		updated, e = tx.Repos.Upsert(ctx, orgID, domain.Repository{
			Owner: "acme", Repo: "api",
			ProfileText: "v2", DefaultBranch: "main",
			ProfiledAt: &profiled,
		})
		return e
	})
	dbtest.AssertWriteReturnedStoredRow(t, "Upsert (update arm, app pool)", updated, read(created.ID))
	if updated.ID != created.ID || updated.ExternalID != "1296269" {
		t.Errorf("update arm returned %+v, want the existing row with its stored id", updated)
	}

	write("UpdateBaseBranch", func(tx db.TxStores) error {
		var e error
		branched, e = tx.Repos.UpdateBaseBranch(ctx, orgID, created.ID, "develop")
		return e
	})
	dbtest.AssertWriteReturnedStoredRow(t, "UpdateBaseBranch (app pool)", branched, read(created.ID))

	var stamped *domain.Repository
	write("UpdateCloneStatusByRef", func(tx db.TxStores) error {
		var e error
		stamped, e = tx.Repos.UpdateCloneStatusByRef(ctx, orgID, domain.RepoRef{Owner: "acme", Repo: "api"}, "failed", "boom", "ssh")
		return e
	})
	if stamped == nil {
		t.Fatal("UpdateCloneStatusByRef returned no row for a repository the caller can see")
	}
	dbtest.AssertWriteReturnedStoredRow(t, "UpdateCloneStatusByRef (app pool)", *stamped, read(created.ID))
}

func repoIDs(profiles []domain.Repository) []string {
	out := make([]string, len(profiles))
	for i, p := range profiles {
		out[i] = p.Slug()
	}
	return out
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
