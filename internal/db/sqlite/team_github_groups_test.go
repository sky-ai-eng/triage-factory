package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

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

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamA, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "backend"},
		{OrgLogin: "acme", TeamSlug: "frontend"},
		{OrgLogin: "acme", TeamSlug: "platform"},
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamB, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "platform"},
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	for _, slug := range []string{"backend", "frontend", "platform"} {
		teams, err := stores.TeamGitHubGroups.TeamsForGroupSystem(ctx, runmode.LocalDefaultOrgID, "acme", slug)
		if err != nil {
			t.Fatalf("TeamsForGroupSystem(%s): %v", slug, err)
		}
		found := false
		for _, id := range teams {
			if id == teamA {
				found = true
			}
		}
		if !found {
			t.Errorf("TeamsForGroupSystem(acme/%s) = %v; want it to include teamA", slug, teams)
		}
	}

	// platform is shared → resolves to both teams.
	teams, err := stores.TeamGitHubGroups.TeamsForGroupSystem(ctx, runmode.LocalDefaultOrgID, "acme", "platform")
	if err != nil {
		t.Fatalf("TeamsForGroupSystem(platform): %v", err)
	}
	if len(teams) != 2 {
		t.Errorf("TeamsForGroupSystem(acme/platform) = %v; want both teams", teams)
	}

	// A login/slug nobody mapped resolves to nothing.
	none, err := stores.TeamGitHubGroups.TeamsForGroupSystem(ctx, runmode.LocalDefaultOrgID, "acme", "ghost")
	if err != nil {
		t.Fatalf("TeamsForGroupSystem(ghost): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("TeamsForGroupSystem(acme/ghost) = %v; want empty", none)
	}
}

// TestTeamGitHubGroups_SQLite_PruneDeletionLifecycle pins the
// GitHub-team-deletion reconcile floor on SQLite: a deleted GitHub team
// drops out of every TF team that referenced it, survivors and the TF
// teams stay.
func TestTeamGitHubGroups_SQLite_PruneDeletionLifecycle(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	teamA := runmode.LocalDefaultTeamID
	teamB := "team-b-0000-0000-0000-000000000002"
	seedExtraTeam(t, conn, teamB, "team-b")

	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamA, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "legacy"},
		{OrgLogin: "acme", TeamSlug: "backend"},
	}); err != nil {
		t.Fatalf("seed teamA: %v", err)
	}
	if err := stores.TeamGitHubGroups.SetForTeam(ctx, teamB, []domain.TeamGitHubGroup{
		{OrgLogin: "acme", TeamSlug: "legacy"},
	}); err != nil {
		t.Fatalf("seed teamB: %v", err)
	}

	// GitHub now reports only "backend" — "legacy" was deleted.
	n, err := stores.TeamGitHubGroups.PruneMissingSystem(ctx, runmode.LocalDefaultOrgID, "acme", []string{"backend"})
	if err != nil {
		t.Fatalf("PruneMissingSystem: %v", err)
	}
	if n != 2 {
		t.Errorf("PruneMissingSystem removed %d rows; want 2 (legacy from both teams)", n)
	}

	a, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, teamA)
	if err != nil {
		t.Fatalf("ListForTeamSystem(teamA): %v", err)
	}
	if len(a) != 1 || a[0].TeamSlug != "backend" {
		t.Errorf("teamA after prune = %+v; want only backend", a)
	}
	b, err := stores.TeamGitHubGroups.ListForTeamSystem(ctx, teamB)
	if err != nil {
		t.Fatalf("ListForTeamSystem(teamB): %v", err)
	}
	if len(b) != 0 {
		t.Errorf("teamB after prune = %+v; want empty (legacy was its only mapping)", b)
	}

	// Both TF teams survive the prune.
	var teamCount int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM teams WHERE org_id = ?`, runmode.LocalDefaultOrgID).Scan(&teamCount); err != nil {
		t.Fatalf("count teams: %v", err)
	}
	if teamCount != 2 {
		t.Errorf("team count = %d; want 2 (prune must never delete TF teams)", teamCount)
	}
}
