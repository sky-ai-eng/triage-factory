package credprovision

import (
	"context"
	"database/sql"
	"maps"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbseed"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
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

	if _, err := database.Exec(`UPDATE repositories SET external_id = '4242' WHERE id = ?`, widgets); err != nil {
		t.Fatalf("record acme/widgets' GitHub id: %v", err)
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
	// The gh channel's repository gate matches by GitHub's id, sealed beside
	// the name.
	if len(gh.RepoIDs) != 1 || gh.RepoIDs["acme/renamed"] != "4242" {
		t.Errorf("RepoIDs = %v, want acme/renamed's GitHub id alone", gh.RepoIDs)
	}
}

// The authorized set is sealed whole, so a repository whose token the
// credential could not mint is still one the gh channel's gate admits.
func TestManager_resolveGitHub_SealsTheWholeSetsIDs(t *testing.T) {
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
	if _, err := database.Exec(`UPDATE repositories SET external_id = '7' WHERE id = ?`, widgets); err != nil {
		t.Fatalf("record acme/widgets' GitHub id: %v", err)
	}
	if err := stores.TeamGitHubRepos.ReplaceForTeam(ctx, org, team, host, []string{widgets, gears}); err != nil {
		t.Fatalf("track repos: %v", err)
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
			token:   githubapp.Token{Value: "ghp_org", ExpiresAt: time.Now().Add(time.Hour)},
			errFor:  map[string]error{"acme/gears": ghclient.ErrNoGitHubCredentials},
		},
	}
	// No anchor, so the set is the team's tracked repositories.
	gh, err := m.resolveGitHub(ctx, org, team, "", "")
	if err != nil || gh == nil {
		t.Fatalf("resolveGitHub = %v, %v", gh, err)
	}
	if _, minted := gh.RepoTokens["acme/gears"]; minted {
		t.Fatalf("RepoTokens = %v, want no token for acme/gears", gh.RepoTokens)
	}
	want := map[string]string{"acme/widgets": "7", "acme/gears": ""}
	if !maps.Equal(gh.RepoIDs, want) {
		t.Errorf("RepoIDs = %v, want %v", gh.RepoIDs, want)
	}
}
