package sqlite_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedExtraTeam inserts a second team in the local sentinel org so the
// multi-team routing + deletion cases have more than the N=1 default to
// work with. SQLite is unscoped (no RLS), so a raw INSERT is enough.
func seedExtraTeam(t *testing.T, conn *sql.DB, id, slug string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, ?, ?)`,
		id, runmode.LocalDefaultOrgID, slug, slug,
	); err != nil {
		t.Fatalf("seed team %s: %v", slug, err)
	}
}

// seedGroupWithoutIDs inserts a mapping the way the migration copies one
// stored before GitHub's ids were recorded: an org login and team slug, and
// neither id.
func seedGroupWithoutIDs(t *testing.T, conn *sql.DB, teamID, login, slug string) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO team_github_groups (team_id, host, github_org_login, github_team_slug) VALUES (?, ?, ?, ?)`,
		teamID, dbtest.TestGitHubHost, login, slug,
	); err != nil {
		t.Fatalf("seed group %s/%s without ids: %v", login, slug, err)
	}
}

func group(teamID, orgID, login, slug string) domain.TeamGitHubGroup {
	return domain.TeamGitHubGroup{GitHubTeamID: teamID, GitHubOrgID: orgID, OrgLogin: login, TeamSlug: slug}
}

// TestTeamGitHubGroups_SQLite_ManyToOneRouting pins the "many GitHub
// teams → one TF team" funnel plus the M:N reverse on the SQLite
// backend (so the routing acceptance holds in local mode without
// Docker).
func TestTeamGitHubGroups_SQLite_ManyToOneRouting(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := "team-b-0000-0000-0000-000000000001"
	seedExtraTeam(t, conn, teamB, "team-b")

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		group("11", "9", "acme", "backend"),
		group("12", "9", "acme", "frontend"),
		group("13", "9", "acme", "platform"),
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamB, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		group("13", "9", "acme", "platform"),
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	for _, id := range []string{"11", "12", "13"} {
		teams, err := stores.TeamGitHubGroups.TeamsForGitHubTeamSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, id, "", "")
		if err != nil {
			t.Fatalf("TeamsForGitHubTeamSystem(%s): %v", id, err)
		}
		found := false
		for _, tid := range teams {
			if tid == teamA {
				found = true
			}
		}
		if !found {
			t.Errorf("TeamsForGitHubTeamSystem(%s) = %v; want it to include teamA", id, teams)
		}
	}

	// platform is shared → resolves to both teams.
	teams, err := stores.TeamGitHubGroups.TeamsForGitHubTeamSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, "13", "acme", "platform")
	if err != nil {
		t.Fatalf("TeamsForGitHubTeamSystem(platform): %v", err)
	}
	if len(teams) != 2 {
		t.Errorf("TeamsForGitHubTeamSystem(13) = %v; want both teams", teams)
	}

	// A team nobody mapped resolves to nothing.
	none, err := stores.TeamGitHubGroups.TeamsForGitHubTeamSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, "99", "acme", "ghost")
	if err != nil {
		t.Fatalf("TeamsForGitHubTeamSystem(ghost): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("TeamsForGitHubTeamSystem(99) = %v; want empty", none)
	}
}

// TestTeamGitHubGroups_SQLite_ReconcileDeletionLifecycle pins the
// GitHub-team-deletion reconcile on SQLite: a deleted GitHub team drops out
// of every TF team that referenced it, survivors and the TF teams stay.
func TestTeamGitHubGroups_SQLite_ReconcileDeletionLifecycle(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := "team-b-0000-0000-0000-000000000002"
	seedExtraTeam(t, conn, teamB, "team-b")

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamA, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		group("14", "9", "acme", "legacy"),
		group("11", "9", "acme", "backend"),
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamB, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		group("14", "9", "acme", "legacy"),
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	// GitHub now reports only "backend" — "legacy" was deleted.
	n, err := stores.TeamGitHubGroups.ReconcileOrgTeamsSystem(ctx, runmode.LocalDefaultOrgID, dbtest.TestGitHubHost, domain.GitHubOrgTeams{
		OrgID: "9", OrgLogin: "acme", Teams: []domain.GitHubTeam{{ID: "11", Slug: "backend"}},
	})
	if err != nil {
		t.Fatalf("ReconcileOrgTeamsSystem: %v", err)
	}
	if n != 2 {
		t.Errorf("ReconcileOrgTeamsSystem deleted %d rows; want 2 (legacy from both teams)", n)
	}

	a, err := stores.TeamGitHubGroups.ListForTeam(ctx, teamA, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam(teamA): %v", err)
	}
	if len(a) != 1 || a[0].TeamSlug != "backend" {
		t.Errorf("teamA after reconcile = %+v; want only backend", a)
	}
	b, err := stores.TeamGitHubGroups.ListForTeam(ctx, teamB, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam(teamB): %v", err)
	}
	if len(b) != 0 {
		t.Errorf("teamB after reconcile = %+v; want empty (legacy was its only mapping)", b)
	}

	// Both TF teams survive the reconcile.
	var teamCount int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM teams WHERE org_id = ?`, runmode.LocalDefaultOrgID).Scan(&teamCount); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	if teamCount != 2 {
		t.Errorf("team count = %d; want 2 (the reconcile must never delete TF teams)", teamCount)
	}
}

// A mapping stored before GitHub's ids were recorded routes by its handle
// until the reconcile identifies it, which it does once from the
// organization's listing; it is never deleted while unidentified.
func TestTeamGitHubGroups_SQLite_RowsWithoutIDs(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	seedGroupWithoutIDs(t, conn, team, "acme", "backend")
	seedGroupWithoutIDs(t, conn, team, "acme", "gone")
	seedGroupWithoutIDs(t, conn, team, "beta", "platform")

	teamsFor := func(id, login, slug string) []string {
		t.Helper()
		got, err := stores.TeamGitHubGroups.TeamsForGitHubTeamSystem(ctx, org, dbtest.TestGitHubHost, id, login, slug)
		if err != nil {
			t.Fatalf("TeamsForGitHubTeamSystem(%q, %s/%s): %v", id, login, slug, err)
		}
		return got
	}
	// Before the reconcile: a review request carrying the team's id and its
	// handle routes through the handle, as a mention does.
	if got := teamsFor("11", "Acme", "Backend"); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an unidentified row by id + handle = %v; want [%s]", got, team)
	}
	if got := teamsFor("", "acme", "backend"); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an unidentified row by handle = %v; want [%s]", got, team)
	}

	// The listing identifies backend; "gone" is not in it but has no id to
	// be absent by, so it stays; beta's row is another organization's.
	n, err := stores.TeamGitHubGroups.ReconcileOrgTeamsSystem(ctx, org, dbtest.TestGitHubHost, domain.GitHubOrgTeams{
		OrgID: "9", OrgLogin: "acme", Teams: []domain.GitHubTeam{{ID: "11", Slug: "backend"}, {ID: "12", Slug: "frontend"}},
	})
	if err != nil {
		t.Fatalf("ReconcileOrgTeamsSystem: %v", err)
	}
	if n != 0 {
		t.Errorf("ReconcileOrgTeamsSystem deleted %d rows; want 0 — an unidentified row is never deleted", n)
	}
	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	want := []domain.TeamGitHubGroup{
		group("11", "9", "acme", "backend"),
		group("", "", "acme", "gone"),
		group("", "", "beta", "platform"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after identifying, groups = %+v; want %+v", got, want)
	}

	// Identified, the row follows GitHub's renames and its id alone routes it.
	if _, err := stores.TeamGitHubGroups.ReconcileOrgTeamsSystem(ctx, org, dbtest.TestGitHubHost, domain.GitHubOrgTeams{
		OrgID: "9", OrgLogin: "acme-corp", Teams: []domain.GitHubTeam{{ID: "11", Slug: "server"}},
	}); err != nil {
		t.Fatalf("ReconcileOrgTeamsSystem (renamed): %v", err)
	}
	if got := teamsFor("11", "", ""); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an identified row by id = %v; want [%s]", got, team)
	}
	if got := teamsFor("", "acme-corp", "server"); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an identified row by its refreshed handle = %v; want [%s]", got, team)
	}
}

// An unidentified row whose team the same TF team already maps by id is a
// duplicate: the reconcile drops it rather than give two rows one id.
func TestTeamGitHubGroups_SQLite_IdentifyingADuplicateDropsIt(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	if _, err := conn.Exec(
		`INSERT INTO team_github_groups (team_id, host, github_team_id, github_org_id, github_org_login, github_team_slug) VALUES (?, ?, '11', '9', 'acme', 'backend')`,
		team, dbtest.TestGitHubHost,
	); err != nil {
		t.Fatalf("seed identified row: %v", err)
	}
	seedGroupWithoutIDs(t, conn, team, "acme", "backend")

	if _, err := stores.TeamGitHubGroups.ReconcileOrgTeamsSystem(ctx, org, dbtest.TestGitHubHost, domain.GitHubOrgTeams{
		OrgID: "9", OrgLogin: "acme", Teams: []domain.GitHubTeam{{ID: "11", Slug: "backend"}},
	}); err != nil {
		t.Fatalf("ReconcileOrgTeamsSystem: %v", err)
	}
	got, err := stores.TeamGitHubGroups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil {
		t.Fatalf("ListForTeam: %v", err)
	}
	if want := []domain.TeamGitHubGroup{group("11", "9", "acme", "backend")}; !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %+v; want %+v", got, want)
	}
}
