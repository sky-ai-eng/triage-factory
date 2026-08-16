package sqlite_test

import (
	"context"
	"sort"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTeamGitHubRepos_SQLite_TrackedSetIsTheUnion is the local-backend proof
// that the tracked set — what the poller and the profiler work through — is
// the union of every team's rows, and that untracking is a delete on THIS
// table only. The registry row a repository got when it was first tracked
// stays, because a worktree ledger entry, a pinned project or a task may still
// name it; the tracked set is what shrinks.
func TestTeamGitHubRepos_SQLite_TrackedSetIsTheUnion(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := "team-b-0000-0000-0000-000000000001"
	seedExtraTeam(t, conn, teamB, "team-b")

	tracked := func() []string {
		t.Helper()
		names, err := stores.Repos.ListTrackedNamesSystem(ctx, runmode.LocalDefaultOrgID)
		if err != nil {
			t.Fatalf("ListTrackedNamesSystem: %v", err)
		}
		sort.Strings(names)
		return names
	}
	registry := func() []string {
		t.Helper()
		names, err := stores.Repos.ListConfiguredNames(ctx, runmode.LocalDefaultOrgID)
		if err != nil {
			t.Fatalf("ListConfiguredNames: %v", err)
		}
		sort.Strings(names)
		return names
	}

	// Team A tracks two repos → both materialize in repositories.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "api"},
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA replace: %v", err)
	}
	if got, want := tracked(), []string{"acme/api", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA: tracked = %v, want %v", got, want)
	}

	// Team B also tracks acme/web (shared) plus acme/infra.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "web"},
		{Owner: "acme", Repo: "infra"},
	}); err != nil {
		t.Fatalf("teamB replace: %v", err)
	}
	if got, want := tracked(), []string{"acme/api", "acme/infra", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamB: tracked = %v, want %v", got, want)
	}

	// Team A drops acme/api. No other team tracks it, so it leaves the tracked
	// set — and its registry row stays exactly where it was.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA, []domain.TeamGitHubRepo{
		{Owner: "acme", Repo: "web"},
	}); err != nil {
		t.Fatalf("teamA shrink: %v", err)
	}
	if got, want := tracked(), []string{"acme/infra", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA shrink: tracked = %v, want %v (acme/api untracked, acme/web survives via teamB)", got, want)
	}
	if got, want := registry(), []string{"acme/api", "acme/infra", "acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamA shrink: registry = %v, want %v — untracking is not a delete", got, want)
	}

	// Team B clears everything → only acme/web (still on team A) is tracked.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamB, nil); err != nil {
		t.Fatalf("teamB clear: %v", err)
	}
	if got, want := tracked(), []string{"acme/web"}; !equalSlugs(got, want) {
		t.Fatalf("after teamB clear: tracked = %v, want %v", got, want)
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

// TestTeamGitHubRepos_SQLite_CasingChangePreservesCache pins the registry
// contract under a casing-only change: re-tracking the same GitHub repo with
// different casing must NOT delete + reinsert the repositories row (which would
// drop cached profile_text / base_branch / clone status / etag). The row is
// matched case-insensitively and kept.
func TestTeamGitHubRepos_SQLite_CasingChangePreservesCache(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	teamA := runmode.LocalDefaultTeamID

	// Track acme/API, then populate cached columns on its repository row.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA,
		[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "API"}}); err != nil {
		t.Fatalf("initial track: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE repositories SET base_branch = 'main', profile_text = 'cached' WHERE owner = 'acme' AND repo = 'API'`); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Re-track the same repo with different casing.
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, teamA,
		[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}}); err != nil {
		t.Fatalf("recased track: %v", err)
	}

	// Exactly one repository row, casing sticky to the original, cache intact.
	var (
		n            int
		slug, branch string
		text         string
	)
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM repositories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("repositories row count = %d, want 1 (casing change must not duplicate)", n)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT owner || '/' || repo, base_branch, profile_text FROM repositories`).Scan(&slug, &branch, &text); err != nil {
		t.Fatalf("read repository row: %v", err)
	}
	if slug != "acme/API" {
		t.Errorf("slug = %q, want sticky original casing acme/API", slug)
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

// Tracking is what brings a repository into the repositories table — not
// profiling — so the row tracking mints has to be a complete identity row from
// the moment it exists. The store's own get-or-create and this path share one
// INSERT precisely so a row cannot differ by which of them created it.
func TestTeamGitHubRepos_SQLite_TrackedRowCarriesIdentity(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID,
		[]domain.TeamGitHubRepo{{Owner: "acme", Repo: "api"}}); err != nil {
		t.Fatalf("ReplaceForTeam: %v", err)
	}

	got, err := stores.Repos.Get(ctx, runmode.LocalDefaultOrgID, "acme/api")
	if err != nil || got == nil {
		t.Fatalf("Get after tracking: got=%v err=%v — tracking must create the row immediately", got, err)
	}
	if got.Source != domain.RepoSourceGitHub {
		t.Errorf("source = %q, want %q", got.Source, domain.RepoSourceGitHub)
	}
	if got.ExternalID != "" {
		t.Errorf("external id = %q, want empty — tracking learns no id, and none is invented", got.ExternalID)
	}
	if got.ProfiledAt != nil {
		t.Error("the freshly tracked row is already profiled; it should be bare until the profiler runs")
	}

	// Get-or-create over the row tracking just made is a read: same row, no
	// second one.
	again, err := stores.Repos.GetOrCreateSystem(ctx, runmode.LocalDefaultOrgID,
		domain.RepoRef{Owner: "acme", Repo: "api"})
	if err != nil || again == nil {
		t.Fatalf("GetOrCreateSystem: got=%v err=%v", again, err)
	}
	if again.ID != got.ID {
		t.Errorf("get-or-create returned id %q, want the tracked row's %q", again.ID, got.ID)
	}
	if n, _ := stores.Repos.CountConfigured(ctx, runmode.LocalDefaultOrgID); n != 1 {
		t.Errorf("repositories rows = %d, want 1", n)
	}
}
