package agenthost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestRecordExternalWrite_LinksTheActionToWhatItWasAbout pins the ledger's
// references as the recording funnel writes them: an act about a pull request
// references its entity (resolved, or created, before the write), an act about
// a repository with no entity references the registry row its owner/repo
// names, and an act naming no registered repository, no repository at all, or
// no GitHub object references nothing. A lookup never registers a repository,
// and references a relayed row carries are replaced by what this side
// resolves.
func TestRecordExternalWrite_LinksTheActionToWhatItWasAbout(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		t.Run(map[bool]string{true: "event-triggered", false: "manual"}[eventTriggered], func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			ctx := context.Background()
			repoID := registerRepo(t, stores, "octo", "repo", "1")

			record := func(key string, act domain.ExternalAction) {
				t.Helper()
				act.DedupKey = key
				if act.Provider == "" {
					act.Provider, act.Credential = domain.ProviderGitHub, domain.CredentialGitHubApp
				}
				RecordExternalWrite(ctx, stores, info, nil, &act)
			}
			record("comment", domain.ExternalAction{Action: domain.ActionCommentPosted, Target: "octo/repo#5"})
			record("settings", domain.ExternalAction{Action: domain.ActionRepoEdited, Target: "octo/repo"})
			record("unregistered", domain.ExternalAction{Action: domain.ActionGitDenied, Target: "octo/elsewhere"})
			record("graphql", domain.ExternalAction{Action: domain.ActionGraphQLWrite, Target: "PR_kwDOnode"})
			record("path", domain.ExternalAction{Action: domain.ActionGHChannelWrite, Target: "/user/keys"})
			record("egress", *egressDeniedAction(info.ConversationID, "evil.example:443", "off-allowlist"))
			record("relayed", domain.ExternalAction{
				Action: domain.ActionLabelDefined, Target: "octo/repo", EntityID: uuid.NewString(), RepositoryID: uuid.NewString(),
			})

			pr, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#5")
			if err != nil || pr == nil {
				t.Fatalf("the comment's pull request entity = %v, %v; want the one the funnel resolved", pr, err)
			}
			want := map[string][2]string{
				"comment":      {pr.ID, ""},
				"settings":     {"", repoID},
				"unregistered": {"", ""},
				"graphql":      {"", ""},
				"path":         {"", ""},
				"egress":       {"", ""},
				"relayed":      {"", repoID},
			}
			acts := listExternalActions(t, stores)
			if len(acts) != len(want) {
				t.Fatalf("recorded %d actions, want %d: %+v", len(acts), len(want), acts)
			}
			for _, a := range acts {
				if got := [2]string{a.EntityID, a.RepositoryID}; got != want[a.DedupKey] {
					t.Errorf("action %s references (entity %q, repository %q), want %q", a.DedupKey, got[0], got[1], want[a.DedupKey])
				}
			}
			if r, err := stores.Repos.GetByRefSystem(ctx, info.OrgID, domain.RepoRefFromSlug(testScope("github"), "octo/elsewhere")); err != nil || r != nil {
				t.Errorf("octo/elsewhere = %+v, %v; want no registry row — a lookup never registers one", r, err)
			}
		})
	}
}
