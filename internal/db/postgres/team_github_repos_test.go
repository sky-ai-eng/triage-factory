package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestTeamGitHubRepos_ReplaceForTeam_AppPath exercises the production
// write path on real Postgres: team admins saving repos through the
// claims-bound app pool (RLS active), with ReplaceForTeam's in-tx
// reconcile reading the cross-team union via the tf.org_tracked_repos()
// SECURITY DEFINER helper and rewriting the org-shared repo_profiles
// cache atomically. Proves the derived-cache contract end-to-end under
// real RLS: add materializes a profile row, last-tracker drop GCs it, a
// repo another team still tracks survives.
func TestTeamGitHubRepos_ReplaceForTeam_AppPath(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AppDB)

	repoProfiles := func() []string {
		t.Helper()
		rows, err := h.AdminDB.Query(
			`SELECT owner || '/' || repo FROM repo_profiles WHERE org_id = $1 ORDER BY 1`, orgA)
		if err != nil {
			t.Fatalf("read repo_profiles: %v", err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}

	save := func(user, team string, repos ...domain.TeamGitHubRepo) {
		t.Helper()
		if err := stores.Tx.WithTx(ctx, orgA, user, func(tx db.TxStores) error {
			return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, team, repos)
		}); err != nil {
			t.Fatalf("ReplaceForTeam(%s): %v", team, err)
		}
	}

	// alice (teamA admin) tracks two repos → both materialize.
	save(alice, teamA, domain.TeamGitHubRepo{Owner: "acme", Repo: "api"}, domain.TeamGitHubRepo{Owner: "acme", Repo: "web"})
	if got := repoProfiles(); !pgStrsEqual(got, []string{"acme/api", "acme/web"}) {
		t.Fatalf("after alice: repo_profiles = %v, want [acme/api acme/web]", got)
	}

	// carol (teamB admin) tracks acme/web (shared) + acme/infra.
	save(carol, teamB, domain.TeamGitHubRepo{Owner: "acme", Repo: "web"}, domain.TeamGitHubRepo{Owner: "acme", Repo: "infra"})
	if got := repoProfiles(); !pgStrsEqual(got, []string{"acme/api", "acme/infra", "acme/web"}) {
		t.Fatalf("after carol: repo_profiles = %v, want all three", got)
	}

	// alice drops acme/api (last tracker → GC'd), keeps acme/web (carol
	// still tracks it → survives).
	save(alice, teamA, domain.TeamGitHubRepo{Owner: "acme", Repo: "web"})
	if got := repoProfiles(); !pgStrsEqual(got, []string{"acme/infra", "acme/web"}) {
		t.Fatalf("after alice shrink: repo_profiles = %v, want [acme/infra acme/web] (acme/api GC'd, acme/web survives via teamB)", got)
	}
}

func pgStrsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
