package sqlite_test

import (
	"context"
	"sort"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamGitHubRepos_SQLite_ReconcilesRepoProfiles is the local-backend
// proof of the derived-cache contract: adding a repo to a team creates
// the repo_profiles row; removing it from the *last* team that tracked it
// GCs the row; a repo still tracked by another team survives.
func TestTeamGitHubRepos_SQLite_ReconcilesRepoProfiles(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := "team-b-0000-0000-0000-000000000001"
	seedExtraTeam(t, conn, teamB, "team-b")

	profiles := func() []string {
		t.Helper()
		names, err := stores.Repos.ListConfiguredNames(ctx, runmode.LocalDefaultOrgID)
		if err != nil {
			t.Fatalf("ListConfiguredNames: %v", err)
		}
		sort.Strings(names)
		return names
	}

	// Team A tracks two repos → both materialize in repo_profiles.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA replace: %v", err)
	}
	if got, want := profiles(), []string{"acme/api", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA: repo_profiles = %v, want %v", got, want)
	}

	// Team B also tracks acme/web (shared) plus acme/infra.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, teamB, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "web"},
		{Owner: "acme", Repo: "infra"},
	}); err != nil {
		t.Fatalf("teamB replace: %v", err)
	}
	if got, want := profiles(), []string{"acme/api", "acme/infra", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamB: repo_profiles = %v, want %v", got, want)
	}

	// Team A drops acme/api (no other team tracks it → GC'd) and keeps
	// acme/web (team B still tracks it → survives).
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA shrink: %v", err)
	}
	if got, want := profiles(), []string{"acme/infra", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA shrink: repo_profiles = %v, want %v (acme/api should be GC'd, acme/web survives via teamB)", got, want)
	}

	// Team B clears everything → only acme/web (still on team A) survives;
	// acme/infra was last-tracked by B and is GC'd.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, teamB, nil); err != nil {
		t.Fatalf("teamB clear: %v", err)
	}
	if got, want := profiles(), []string{"acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamB clear: repo_profiles = %v, want %v", got, want)
	}

	// ListForOrgSystem reports the same union.
	union, err := stores.TeamGitHubRepos.ListForOrgSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListForOrgSystem: %v", err)
	}
	if len(union) != 1 || union[0].Slug() != "acme/web" {
		t.Fatalf("ListForOrgSystem = %v, want [acme/web]", union)
	}
}

// TestTeamGitHubRepos_SQLite_TracksRepo pins the router-gate lookup,
// including the case-insensitive owner match.
func TestTeamGitHubRepos_SQLite_TracksRepo(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID

	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, teamA, []domain.TeamGitHubRepo{
		{Owner: "Acme", Repo: "api"},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	cases := []struct {
		owner, repo string
		want        bool
	}{
		{"Acme", "api", true},
		{"acme", "api", true},  // owner matched case-insensitively
		{"ACME", "api", true},  // ditto
		{"acme", "web", false}, // untracked repo
		{"other", "api", false},
	}
	for _, c := range cases {
		got, err := stores.TeamGitHubRepos.TracksRepoSystem(ctx, teamA, c.owner, c.repo)
		if err != nil {
			t.Fatalf("TracksRepoSystem(%s/%s): %v", c.owner, c.repo, err)
		}
		if got != c.want {
			t.Errorf("TracksRepoSystem(%s/%s) = %v, want %v", c.owner, c.repo, got, c.want)
		}
	}

	// A different team tracks nothing.
	teamB := "team-b-0000-0000-0000-000000000002"
	seedExtraTeam(t, conn, teamB, "team-b")
	if got, _ := stores.TeamGitHubRepos.TracksRepoSystem(ctx, teamB, "Acme", "api"); got {
		t.Error("teamB should not track acme/api")
	}
}

func equalSlugs(a, b []string) bool {
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
