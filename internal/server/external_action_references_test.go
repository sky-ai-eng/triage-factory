package server

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestLinkPullRequestAction_ResolvesThePathsNameOnce pins the references a
// dashboard audit row about owner/repo#N takes: the entity of that pull
// request in the live repository holding the name, the repository's registry
// row when TF holds no entity for the pull request, and nothing for a name no
// registry row answers to. A closed entity of a retired repository that held
// the name is never taken for the live one's pull request.
func TestLinkPullRequestAction_ResolvesThePathsNameOnce(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()
	stores := sqlitestore.New(srv.db)
	org := runmode.LocalDefaultOrgID
	host := dbtest.TestGitHubHost

	dead, err := stores.Repos.UpsertSystem(ctx, org, domain.Repository{Host: host, Owner: "acme", Repo: "api", ExternalID: "1"})
	if err != nil {
		t.Fatalf("seed the retired repository: %v", err)
	}
	if _, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", host, "acme/api#9", "PR_dead9", "pr", "Dead #9", ""); err != nil {
		t.Fatalf("seed the retired repository's #9: %v", err)
	}
	if err := dbtest.RetireRepo(ctx, stores.Repos, org, host, "acme/api"); err != nil {
		t.Fatal(err)
	}
	live, err := stores.Repos.UpsertSystem(ctx, org, domain.Repository{Host: host, Owner: "acme", Repo: "api", ExternalID: "2"})
	if err != nil || live.ID == dead.ID {
		t.Fatalf("seed the live repository: %+v, %v", live, err)
	}
	pr7, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", host, "acme/api#7", "PR_live7", "pr", "Live #7", "")
	if err != nil {
		t.Fatalf("seed the live repository's #7: %v", err)
	}

	for _, c := range []struct {
		name, owner, repo string
		number            int
		want              [2]string
	}{
		{"an entity TF holds", "acme", "API", 7, [2]string{pr7.ID, ""}},
		{"a pull request TF holds no live entity for", "acme", "api", 9, [2]string{"", live.ID}},
		{"a name no registry row answers to", "acme", "gone", 1, [2]string{"", ""}},
	} {
		act := domain.ExternalAction{Action: domain.ActionPRConvertedToDraft}
		linkPullRequestAction(ctx, srv.tx, org, runmode.LocalDefaultUserID, host, c.owner, c.repo, c.number, &act)
		if got := [2]string{act.EntityID, act.RepositoryID}; got != c.want {
			t.Errorf("%s: references (entity %q, repository %q), want %q", c.name, got[0], got[1], c.want)
		}
	}
}
