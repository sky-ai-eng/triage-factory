package agenthost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRecordExternalWrite_LinksTheActionToWhatItWasAbout pins the ledger's
// references as the recording funnel writes them: an act about a pull request
// references its entity (which the funnel resolves, or creates, before the
// write), an act about a repository with no entity references the registry
// row its owner/repo names, and an act naming no registered repository, no
// repository at all, or no GitHub object references nothing. A lookup never
// registers a repository, and references a relayed row carries are replaced
// by what this side resolves.
func TestRecordExternalWrite_LinksTheActionToWhatItWasAbout(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			ctx := context.Background()
			registerRepo(t, stores, "octo", "repo", "1")
			repo, err := stores.Repos.GetByRefSystem(ctx, info.OrgID, domain.RepoRefFromSlug(testScope("github"), "octo/repo"))
			if err != nil || repo == nil {
				t.Fatalf("read octo/repo's registry row: %v, %v", repo, err)
			}

			record := func(key string, act domain.ExternalAction) {
				t.Helper()
				act.DedupKey = key
				if act.Credential == "" {
					act.Credential = domain.CredentialGitHubApp
				}
				RecordExternalWrite(ctx, stores, info, nil, &act)
			}
			record("comment", domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Action: domain.ActionCommentPosted, Target: "octo/repo#5"})
			record("settings", domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Action: domain.ActionRepoEdited, Target: "octo/repo"})
			record("unregistered", domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Action: domain.ActionGitDenied, Target: "octo/elsewhere"})
			record("graphql", domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Action: domain.ActionGraphQLWrite, Target: "PR_kwDOnode"})
			record("path", domain.ExternalAction{Provider: domain.ArtifactProviderGitHub, Action: domain.ActionGHChannelWrite, Target: "/user/keys"})
			record("egress", *egressDeniedAction(info.ConversationID, "evil.example:443", "off-allowlist"))
			record("relayed", domain.ExternalAction{
				Provider: domain.ArtifactProviderGitHub, Action: domain.ActionLabelDefined, Target: "octo/repo",
				EntityID: uuid.NewString(), RepositoryID: uuid.NewString(),
			})

			pr, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/repo#5")
			if err != nil || pr == nil {
				t.Fatalf("the comment's pull request entity = %v, %v; want the one the funnel resolved", pr, err)
			}
			want := map[string][2]string{
				"comment":      {pr.ID, ""},
				"settings":     {"", repo.ID},
				"unregistered": {"", ""},
				"graphql":      {"", ""},
				"path":         {"", ""},
				"relayed":      {"", repo.ID},
			}
			got := map[string][2]string{}
			for _, a := range listExternalActions(t, stores) {
				got[a.DedupKey] = [2]string{a.EntityID, a.RepositoryID}
				if a.Action == domain.ActionEgressDenied && (a.EntityID != "" || a.RepositoryID != "") {
					t.Errorf("egress denial references (%q, %q), want nothing", a.EntityID, a.RepositoryID)
				}
			}
			for key, refs := range want {
				if got[key] != refs {
					t.Errorf("action %s references (entity %q, repository %q), want %q", key, got[key][0], got[key][1], refs)
				}
			}
			if r, err := stores.Repos.GetByRefSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRefFromSlug(testScope("github"), "octo/elsewhere")); err != nil || r != nil {
				t.Errorf("octo/elsewhere = %+v, %v; want no registry row — a lookup never registers one", r, err)
			}
		})
	}
}
