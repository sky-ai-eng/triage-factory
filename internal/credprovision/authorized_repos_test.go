package credprovision

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A checkout authorizes the repository its ledger row's registry id names,
// and tracking is checked on that row: a renamed row authorizes its checkout
// under the name it carries now, and a retired row authorizes nothing, even
// though the repository GitHub now serves under its name has a row the team
// tracks.
func TestManager_resolveGitHub_LedgerRowsAreCheckedByRegistryID(t *testing.T) {
	ctx := context.Background()
	org, team, host := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, dbtest.TestGitHubHost
	database, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	stores := sqlitestore.New(database)

	widgets := dbseed.GitHubRepository(t, database, org, host, "acme", "widgets")
	gears := dbseed.GitHubRepository(t, database, org, host, "acme", "gears")
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, org, team, host, []string{widgets, gears}); err != nil {
		t.Fatalf("track repos: %v", err)
	}
	const conversationID = "3c9d6e0a-8f1b-4c2d-9e7f-5a6b7c8d9e0f"
	dbtest.SeedConversation(t, database, domain.Conversation{ID: conversationID})
	for _, id := range []string{widgets, gears} {
		if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, org, domain.ConversationWorktree{
			ConversationID: conversationID, RepositoryID: id, Ref: "default", Path: "/runs/" + id,
		}); err != nil {
			t.Fatalf("record checkout of %s: %v", id, err)
		}
	}

	dbseed.RenameRepositoryOnly(t, database, widgets, "acme", "renamed")
	if _, err := database.Exec(`UPDATE repositories SET retired_at = CURRENT_TIMESTAMP WHERE id = ?`, gears); err != nil {
		t.Fatalf("retire acme/gears: %v", err)
	}
	namesake, err := stores.Repos.FindOrCreateByRefSystem(ctx, org, domain.RepoRefFromSlug(host, "acme/gears"))
	if err != nil || namesake.ID == gears {
		t.Fatalf("register the namesake: row=%+v err=%v; want a row of its own", namesake, err)
	}
	if _, err := database.Exec(`INSERT INTO team_github_repos (team_id, repository_id, org_id) VALUES (?, ?, ?)`, team, namesake.ID, org); err != nil {
		t.Fatalf("track the namesake: %v", err)
	}

	m := &Manager{
		stores: db.Stores{
			Orgs:                  &fakeOrgs{},
			TeamGitHubRepos:       stores.TeamGitHubRepos,
			Repos:                 stores.Repos,
			ConversationWorktrees: stores.ConversationWorktrees,
		},
		ghResolver: &fakeScopedResolver{
			base:    host,
			hasCred: true,
			token:   githubapp.Token{Value: "ghs_scoped", ExpiresAt: time.Now().Add(time.Hour)},
		},
	}
	gh, err := m.resolveGitHub(ctx, org, team, "", conversationID)
	if err != nil || gh == nil {
		t.Fatalf("resolveGitHub = %v, %v", gh, err)
	}
	if _, ok := gh.RepoTokens["acme/renamed"]; !ok || len(gh.RepoTokens) != 1 {
		t.Errorf("RepoTokens = %v, want acme/renamed alone", gh.RepoTokens)
	}
}
