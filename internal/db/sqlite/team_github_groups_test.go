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

// seedGroup inserts a mapping by raw SQL, which unlike SetForTeam takes one
// with no ids: the way the migration copies one stored before GitHub's ids
// were recorded.
func seedGroup(t *testing.T, conn *sql.DB, teamID string, g domain.TeamGitHubGroup) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT INTO team_github_groups (team_id, host, github_team_id, github_org_id, github_org_login, github_team_slug)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		teamID, dbtest.TestGitHubHost, g.GitHubTeamID, g.GitHubOrgID, g.OrgLogin, g.TeamSlug,
	); err != nil {
		t.Fatalf("seed group %+v: %v", g, err)
	}
}

func group(teamID, orgID, login, slug string) domain.TeamGitHubGroup {
	return domain.TeamGitHubGroup{GitHubTeamID: teamID, GitHubOrgID: orgID, OrgLogin: login, TeamSlug: slug}
}

// A mapping stored before GitHub's ids were recorded routes by its handle
// until the reconcile identifies it, which it does once from the
// organization's listing. It is never pruned for its absence from a listing
// while unidentified; one whose team the same TF team already maps by id is a
// duplicate, which the identifying drops rather than give two rows one id.
func TestTeamGitHubGroups_SQLite_RowsWithoutIDs(t *testing.T) {
	conn := openSQLiteForTest(t)
	groups := sqlitestore.New(conn).TeamGitHubGroups
	ctx, org, team := context.Background(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID
	teamsForGitHubTeam := func(id, login, slug string) []string {
		t.Helper()
		got, err := groups.TeamsForGitHubTeamSystem(ctx, org, dbtest.TestGitHubHost, id, login, slug)
		if err != nil {
			t.Fatalf("TeamsForGitHubTeamSystem(%q, %s/%s): %v", id, login, slug, err)
		}
		return got
	}
	for _, g := range []domain.TeamGitHubGroup{
		group("", "", "acme", "backend"),
		group("", "", "acme", "gone"),
		group("", "", "beta", "platform"),
		group("12", "9", "acme", "frontend"),
		group("", "", "acme", "frontend"),
	} {
		seedGroup(t, conn, team, g)
	}

	// Before the reconcile: a review request carrying the team's id and its
	// handle routes through the handle, as a mention does.
	if got := teamsForGitHubTeam("11", "Acme", "Backend"); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an unidentified row by id + handle = %v; want [%s]", got, team)
	}
	if got := teamsForGitHubTeam("", "acme", "backend"); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an unidentified row by handle = %v; want [%s]", got, team)
	}

	// The listing identifies backend; "gone" is not in it but has no id to
	// be absent by, so it stays; beta's row is another organization's.
	if n, err := groups.ReconcileOrgTeamsSystem(ctx, org, dbtest.TestGitHubHost, domain.GitHubOrgTeams{
		OrgID: "9", OrgLogin: "acme", Teams: []domain.GitHubTeam{{ID: "11", Slug: "backend"}, {ID: "12", Slug: "frontend"}},
	}); err != nil || n != 0 {
		t.Errorf("ReconcileOrgTeamsSystem = (%d, %v); want nothing pruned — an unidentified row never is", n, err)
	}
	want := []domain.TeamGitHubGroup{
		group("11", "9", "acme", "backend"),
		group("12", "9", "acme", "frontend"),
		group("", "", "acme", "gone"),
		group("", "", "beta", "platform"),
	}
	if got, err := groups.ListForTeam(ctx, team, dbtest.TestGitHubHost); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("after identifying, groups = %+v, %v; want %+v", got, err, want)
	}

	// Identified, the row follows GitHub's renames and its id alone routes it.
	if _, err := groups.ReconcileOrgTeamsSystem(ctx, org, dbtest.TestGitHubHost, domain.GitHubOrgTeams{
		OrgID: "9", OrgLogin: "acme-corp", Teams: []domain.GitHubTeam{{ID: "11", Slug: "server"}},
	}); err != nil {
		t.Fatalf("ReconcileOrgTeamsSystem (renamed): %v", err)
	}
	if got := teamsForGitHubTeam("11", "", ""); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an identified row by id = %v; want [%s]", got, team)
	}
	if got := teamsForGitHubTeam("", "acme-corp", "server"); !reflect.DeepEqual(got, []string{team}) {
		t.Errorf("an identified row by its refreshed handle = %v; want [%s]", got, team)
	}
}

// A save keeps a mapping without ids that the set names by its handle, as the
// row it is, and deletes one the set leaves out or names by its id too.
// Reading the set back and saving it changes nothing.
func TestTeamGitHubGroups_SQLite_SetForTeamKeepsTheRowsWithoutIDsItNames(t *testing.T) {
	conn := openSQLiteForTest(t)
	groups := sqlitestore.New(conn).TeamGitHubGroups
	ctx, team := context.Background(), runmode.LocalDefaultTeamID
	for _, g := range []domain.TeamGitHubGroup{
		group("", "", "acme", "backend"),
		group("", "", "acme", "gone"),
		group("", "", "beta", "platform"),
	} {
		seedGroup(t, conn, team, g)
	}

	if err := groups.SetForTeam(ctx, team, dbtest.TestGitHubHost, []domain.TeamGitHubGroup{
		group("", "", "ACME", "Backend"),
		group("", "", "beta", "platform"),
		group("13", "8", "beta", "platform"),
		group("12", "9", "acme", "frontend"),
	}); err != nil {
		t.Fatalf("SetForTeam: %v", err)
	}
	want := []domain.TeamGitHubGroup{
		group("", "", "acme", "backend"),
		group("12", "9", "acme", "frontend"),
		group("13", "8", "beta", "platform"),
	}
	got, err := groups.ListForTeam(ctx, team, dbtest.TestGitHubHost)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %+v, %v; want %+v", got, err, want)
	}

	if err := groups.SetForTeam(ctx, team, dbtest.TestGitHubHost, got); err != nil {
		t.Fatalf("SetForTeam(the set as read): %v", err)
	}
	if again, err := groups.ListForTeam(ctx, team, dbtest.TestGitHubHost); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("after saving the set as read, groups = %+v, %v; want %+v", again, err, want)
	}
}
