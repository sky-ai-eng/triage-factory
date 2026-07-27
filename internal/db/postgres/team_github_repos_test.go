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

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)

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

// TestTeamGitHubRepos_Postgres_TracksRepoViewerScoped_RLS pins the
// per-repo, app-pool write/read gate (TFAC-559): under real RLS, a caller
// only "tracks" a repo if one of THEIR teams tracks it — a sibling team
// tracking the same-named repo in the org doesn't count. TracksRepoSystem
// (a specific team, admin pool) is unaffected; this exercises the viewer-
// scoped sibling.
func TestTeamGitHubRepos_Postgres_TracksRepoViewerScoped_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamB, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, teamA, []domain.TeamGitHubRepo{{Owner: "Acme", Repo: "api"}})
	}); err != nil {
		t.Fatalf("track acme/api for teamA: %v", err)
	}

	tracksAs := func(userID, owner, repo string) bool {
		t.Helper()
		var got bool
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgA, owner, repo)
			return e
		}); err != nil {
			t.Fatalf("TracksRepoViewerScoped(%s, %s/%s): %v", userID, owner, repo, err)
		}
		return got
	}

	if !tracksAs(alice, "acme", "api") {
		t.Error("alice's team tracks acme/api; want true")
	}
	if !tracksAs(alice, "ACME", "API") {
		t.Error("match should be case-insensitive")
	}
	if tracksAs(bob, "acme", "api") {
		t.Error("bob's team does not track acme/api; want false even though it exists org-wide")
	}
	if tracksAs(alice, "acme", "ghost") {
		t.Error("acme/ghost is not tracked by anyone; want false")
	}
}

// TestTeamGitHubRepos_Postgres_TracksRepoViewerAdminScoped_RLS pins the
// mutation gate for org-wide repo configuration: seeing a repo (membership)
// and being allowed to change it (team admin) are separate answers. repo
// profiles carry no team_id, so a member of one tracking team writing the
// row changes behaviour for every tracking team — hence the narrower
// predicate.
func TestTeamGitHubRepos_Postgres_TracksRepoViewerAdminScoped_RLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	ctx := context.Background()

	orgA, alice, teamA := pgtest.SeedOrgWithUser(t, h, "alice")
	// bob is a plain member of the SAME team alice admins: he sees
	// acme/api, but must not be able to write it.
	bob := pgtest.SeedUser(t, h, "bob")
	pgtest.AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	// carol admins a different team that tracks nothing — team admin
	// somewhere in the org is not team admin *of a tracking team*.
	teamB := pgtest.SeedTeam(t, h, orgA, "team-b")
	carol := pgtest.SeedUser(t, h, "carol")
	pgtest.AddOrgMember(t, h, carol, orgA, teamB, "member", "admin")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	if err := stores.Tx.WithTx(ctx, orgA, alice, func(tx db.TxStores) error {
		return tx.TeamGitHubRepos.ReplaceForTeam(ctx, orgA, teamA, []domain.TeamGitHubRepo{{Owner: "Acme", Repo: "api"}})
	}); err != nil {
		t.Fatalf("track acme/api for teamA: %v", err)
	}

	adminTracksAs := func(userID, owner, repo string) bool {
		t.Helper()
		var got bool
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, e = tx.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, orgA, owner, repo)
			return e
		}); err != nil {
			t.Fatalf("TracksRepoViewerAdminScoped(%s, %s/%s): %v", userID, owner, repo, err)
		}
		return got
	}
	memberTracksAs := func(userID, owner, repo string) bool {
		t.Helper()
		var got bool
		if err := stores.Tx.WithTx(ctx, orgA, userID, func(tx db.TxStores) error {
			var e error
			got, e = tx.TeamGitHubRepos.TracksRepoViewerScoped(ctx, orgA, owner, repo)
			return e
		}); err != nil {
			t.Fatalf("TracksRepoViewerScoped(%s, %s/%s): %v", userID, owner, repo, err)
		}
		return got
	}

	if !adminTracksAs(alice, "acme", "api") {
		t.Error("alice admins the team tracking acme/api; want true")
	}
	if !adminTracksAs(alice, "ACME", "API") {
		t.Error("match should be case-insensitive")
	}
	if adminTracksAs(alice, "acme", "ghost") {
		t.Error("acme/ghost is tracked by nobody; want false even for a team admin")
	}
	// The split this gate exists for: bob reads the repo, bob cannot write it.
	if !memberTracksAs(bob, "acme", "api") {
		t.Error("bob is a member of the team tracking acme/api; the read gate should pass")
	}
	if adminTracksAs(bob, "acme", "api") {
		t.Error("bob is a plain member, not a team admin; the write gate must reject him")
	}
	if adminTracksAs(carol, "acme", "api") {
		t.Error("carol admins teamB, which doesn't track acme/api; want false")
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
