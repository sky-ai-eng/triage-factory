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
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA replace: %v", err)
	}
	if got, want := profiles(), []string{"acme/api", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA: repo_profiles = %v, want %v", got, want)
	}

	// Team B also tracks acme/web (shared) plus acme/infra.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, []domain.TeamGitHubRepo{
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
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA shrink: %v", err)
	}
	if got, want := profiles(), []string{"acme/infra", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA shrink: repo_profiles = %v, want %v (acme/api should be GC'd, acme/web survives via teamB)", got, want)
	}

	// Team B clears everything → only acme/web (still on team A) survives;
	// acme/infra was last-tracked by B and is GC'd.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, nil); err != nil {
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

// TestTeamGitHubRepos_SQLite_CasingChangePreservesCache pins the
// derived-cache contract under a casing-only change: re-tracking the same
// GitHub repo with different casing must NOT delete + reinsert the
// repo_profiles row (which would drop cached profile_text / base_branch /
// clone status / etag). The row is matched case-insensitively and kept.
func TestTeamGitHubRepos_SQLite_CasingChangePreservesCache(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID

	// Track acme/API, then populate cached columns on its profile row.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA,
		[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "API"}}); err != nil {
		t.Fatalf("initial track: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE repo_profiles SET base_branch = 'main', profile_text = 'cached' WHERE id = 'acme/API'`); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Re-track the same repo with different casing.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA,
		[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}}); err != nil {
		t.Fatalf("recased track: %v", err)
	}

	// Exactly one profile row, casing sticky to the original, cache intact.
	var (
		n          int
		id, branch string
		text       string
	)
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM repo_profiles`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("repo_profiles row count = %d, want 1 (casing change must not duplicate)", n)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT id, base_branch, profile_text FROM repo_profiles`).Scan(&id, &branch, &text); err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if id != "acme/API" {
		t.Errorf("id = %q, want sticky original casing acme/API", id)
	}
	if branch != "main" || text != "cached" {
		t.Errorf("cached columns lost on casing change: base_branch=%q profile_text=%q (want main/cached)", branch, text)
	}
}

// TestTeamGitHubRepos_SQLite_TracksRepo pins the router-gate lookup,
// including the case-insensitive owner match.
func TestTeamGitHubRepos_SQLite_TracksRepo(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID

	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{
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

// TestTeamGitHubRepos_SQLite_TracksRepoViewerScoped pins the local-mode
// asymmetry (TFAC-559): N=1 has no team boundary to enforce, so
// TracksRepoViewerScoped always reports true regardless of whether the
// repo is actually tracked — unlike TracksRepoSystem, which is a real
// per-team lookup. The org-id guard still applies.
func TestTeamGitHubRepos_SQLite_TracksRepoViewerScoped(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	got, err := stores.TeamGitHubRepos.TracksRepoViewerScoped(ctx, runmode.LocalDefaultOrgID, "acme", "never-tracked")
	if err != nil {
		t.Fatalf("TracksRepoViewerScoped: %v", err)
	}
	if !got {
		t.Error("local mode should report true unconditionally (N=1, no team boundary)")
	}

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if _, err := stores.TeamGitHubRepos.TracksRepoViewerScoped(ctx, bogusOrg, "acme", "api"); err == nil {
		t.Error("TracksRepoViewerScoped with non-local orgID should error")
	}
}

// TestTeamGitHubRepos_SQLite_TracksRepoViewerAdminScoped pins the same
// local-mode asymmetry on the mutation gate: N=1 has a single implicit
// owner, so there is no admin/member distinction to enforce and the answer
// is unconditionally true. The org-id guard still applies.
func TestTeamGitHubRepos_SQLite_TracksRepoViewerAdminScoped(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	got, err := stores.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, runmode.LocalDefaultOrgID, "acme", "never-tracked")
	if err != nil {
		t.Fatalf("TracksRepoViewerAdminScoped: %v", err)
	}
	if !got {
		t.Error("local mode should report true unconditionally (N=1, single implicit owner)")
	}

	const bogusOrg = "11111111-1111-1111-1111-111111111111"
	if _, err := stores.TeamGitHubRepos.TracksRepoViewerAdminScoped(ctx, bogusOrg, "acme", "api"); err == nil {
		t.Error("TracksRepoViewerAdminScoped with non-local orgID should error")
	}
}

// TestTeamGitHubRepos_SQLite_ListOrgReposWithTeams pins the
// tracked-repos-with-owning-teams read backing the switch reachability
// preflights (TFAC-328): each repo carries the names of every team tracking
// it, grouped and ordered.
func TestTeamGitHubRepos_SQLite_ListOrgReposWithTeams(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID // seeded name "Default" (or similar)
	teamB := "team-b-0000-0000-0000-000000000003"
	seedExtraTeam(t, conn, teamB, "team-b")

	// teamA tracks api + web; teamB also tracks web (shared) + infra.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA replace: %v", err)
	}
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "web"},
		{Owner: "acme", Repo: "infra"},
	}); err != nil {
		t.Fatalf("teamB replace: %v", err)
	}

	got, err := stores.TeamGitHubRepos.ListOrgReposWithTeamsSystem(ctx, runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListOrgReposWithTeamsSystem: %v", err)
	}
	// Ordered by (owner, repo): api, infra, web.
	if len(got) != 3 {
		t.Fatalf("got %d repos, want 3: %+v", len(got), got)
	}
	bySlug := map[string][]string{}
	for _, r := range got {
		bySlug[r.Slug()] = r.Teams
	}
	if teams := bySlug["acme/web"]; len(teams) != 2 {
		t.Errorf("acme/web teams = %v, want both teams (shared repo)", teams)
	}
	if teams := bySlug["acme/api"]; len(teams) != 1 {
		t.Errorf("acme/api teams = %v, want exactly one", teams)
	}
	if teams := bySlug["acme/infra"]; len(teams) != 1 {
		t.Errorf("acme/infra teams = %v, want exactly one", teams)
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
