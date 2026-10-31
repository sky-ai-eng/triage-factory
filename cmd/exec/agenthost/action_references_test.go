package agenthost

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRecordExternalWrite_LinksTheActionToWhatItWasAbout pins the ledger's
// references as the recording funnel writes them, from the identity the act's
// observer read and never from the act's target. An act about a pull request
// or issue references its entity, found by node id or created under the key
// the registry row's current name builds, with the kind the read answered; an
// act about a repository references the row GitHub's id names; a failed read
// links nothing and creates nothing; a denial's repository is matched by name,
// lookup only; and references a relayed row carries are replaced by what this
// side resolves.
func TestRecordExternalWrite_LinksTheActionToWhatItWasAbout(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		t.Run(map[bool]string{true: "event-triggered", false: "manual"}[eventTriggered], func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			ctx := context.Background()
			org, host := info.OrgID, testScope("github")
			repoID := registerRepo(t, stores, "octo", "repo", "1")
			// A pull request the poller already holds, under its node id.
			polled, _, err := stores.Entities.FindOrCreateSystem(ctx, org, "github", host, "octo/repo#9", "PR_9", "pr", "Polled", "")
			if err != nil {
				t.Fatalf("seed the polled pull request: %v", err)
			}
			repo := domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo"}
			object := func(nodeID, kind string) domain.GitHubIdentity {
				id := repo
				id.NodeID, id.Kind = nodeID, kind
				return id
			}

			record := func(key string, act domain.ExternalAction, id domain.GitHubIdentity) {
				t.Helper()
				act.DedupKey = key
				if act.Provider == "" {
					act.Provider, act.Credential = domain.ProviderGitHub, domain.CredentialGitHubApp
				}
				recordExternalWrite(ctx, stores, info, nil, &act, domain.EntityContainer{}, id)
			}
			// A REST write whose response carried no id: the observer's read
			// supplied the pull request's.
			record("pr", domain.ExternalAction{Action: domain.ActionLabelAdded, Target: "octo/repo#5"}, object("PR_5", domain.EntityKindPullRequest))
			record("issue", domain.ExternalAction{Action: domain.ActionIssueUpdated, Target: "octo/repo#6"}, object("I_6", domain.EntityKindIssue))
			// Spelled under a name the repository had: GitHub's id says which.
			record("old name", domain.ExternalAction{Action: domain.ActionCommentPosted, Target: "octo/old-repo#7"}, object("PR_7", domain.EntityKindPullRequest))
			record("settings", domain.ExternalAction{Action: domain.ActionRepoEdited, Target: "octo/repo"}, repo)
			record("failed read", domain.ExternalAction{Action: domain.ActionPRMerged, Target: "octo/repo#8"}, domain.GitHubIdentity{})
			// A GraphQL write names its subject's node id; nothing was read for
			// it, and the entity carrying it is found under whatever key.
			record("graphql subject", domain.ExternalAction{Action: domain.ActionPRClosed, Target: "PR_9"},
				domain.GitHubIdentity{NodeID: "PR_9", Kind: domain.EntityKindPullRequest})
			record("denied", domain.ExternalAction{Action: domain.ActionGitDenied, Target: "octo/repo"}, domain.GitHubIdentity{})
			record("denied elsewhere", domain.ExternalAction{Action: domain.ActionGitDenied, Target: "octo/elsewhere"}, domain.GitHubIdentity{})
			record("path", domain.ExternalAction{Action: domain.ActionGHChannelWrite, Target: "/user/keys"}, domain.GitHubIdentity{})
			record("egress", *egressDeniedAction(info.ConversationID, "evil.example:443", "off-allowlist"), domain.GitHubIdentity{})
			record("relayed", domain.ExternalAction{
				Action: domain.ActionLabelDefined, Target: "octo/repo", EntityID: uuid.NewString(), RepositoryID: uuid.NewString(),
			}, repo)

			entity := func(key, wantKind, wantNodeID string) string {
				t.Helper()
				e, err := stores.Entities.GetBySourceSystem(ctx, org, "github", host, key)
				if err != nil || e == nil {
					t.Fatalf("entity %s = %v, %v; want the one the funnel resolved", key, e, err)
				}
				if e.Kind != wantKind || e.ExternalID != wantNodeID || e.RepositoryID != repoID {
					t.Errorf("entity %s = kind %q id %q repository %q, want %s %s in %s", key, e.Kind, e.ExternalID, e.RepositoryID, wantKind, wantNodeID, repoID)
				}
				return e.ID
			}
			want := map[string][2]string{
				"pr":               {entity("octo/repo#5", domain.EntityKindPullRequest, "PR_5"), ""},
				"issue":            {entity("octo/repo#6", domain.EntityKindIssue, "I_6"), ""},
				"old name":         {entity("octo/repo#7", domain.EntityKindPullRequest, "PR_7"), ""},
				"settings":         {"", repoID},
				"failed read":      {"", ""},
				"graphql subject":  {polled.ID, ""},
				"denied":           {"", repoID},
				"denied elsewhere": {"", ""},
				"path":             {"", ""},
				"egress":           {"", ""},
				"relayed":          {"", repoID},
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
			for _, key := range []string{"octo/repo#8", "octo/old-repo#7"} {
				if e, err := stores.Entities.GetBySourceSystem(ctx, org, "github", host, key); err != nil || e != nil {
					t.Errorf("entity %s = %+v, %v; want none — nothing is created from a key alone", key, e, err)
				}
			}
			for _, slug := range []string{"octo/elsewhere", "octo/old-repo"} {
				if r, err := stores.Repos.GetByRefSystem(ctx, org, domain.RepoRefFromSlug(host, slug)); err != nil || r != nil {
					t.Errorf("%s = %+v, %v; want no registry row — no repository is registered from a name", slug, r, err)
				}
			}
		})
	}
}

// TestRecordExternalWrite_RegistersAnUnknownRepositoryByItsID: in local mode a
// repository with no registry row is registered by GitHub's id for it, under
// the name GitHub answered with it, and a second act naming it by an old name
// lands on that row.
func TestRecordExternalWrite_RegistersAnUnknownRepositoryByItsID(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	ctx := context.Background()
	id := domain.GitHubIdentity{RepositoryID: "31", Repository: "upstream/lib", NodeID: "PR_lib1", Kind: domain.EntityKindPullRequest}
	for i, target := range []string{"upstream/lib#1", "upstream/old-lib#1"} {
		act := domain.ExternalAction{Provider: domain.ProviderGitHub, Action: domain.ActionLabelAdded, Target: target, Credential: domain.CredentialGitHubApp}
		act.DedupKey = []string{"first", "second"}[i]
		recordExternalWrite(ctx, stores, info, nil, &act, domain.EntityContainer{}, id)
	}
	row, err := stores.Repos.GetByExternalIDSystem(ctx, info.OrgID, testScope("github"), "31")
	if err != nil || row == nil || row.Slug() != "upstream/lib" {
		t.Fatalf("registry row for id 31 = %+v, %v; want upstream/lib", row, err)
	}
	repos, _, err := stores.Repos.List(ctx, info.OrgID, testScope("github"), db.Unwindowed)
	if err != nil || len(repos) != 1 {
		t.Errorf("registry rows = %+v, %v; want the one", repos, err)
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 2 || acts[0].EntityID == "" || acts[0].EntityID != acts[1].EntityID {
		t.Errorf("actions = %+v, want both linked to the one pull request", acts)
	}
}

// TestRecordExternalWrite_RelayedRepositoryOutsideTheRunIsUnlinked: in multi
// mode the identity was read by the run's sidecar and relayed, so a
// repository id links only a repository the run works in — a worktree of the
// run's, or its task's own repository. Any other id records the act unlinked,
// and creates no entity in that repository.
func TestRecordExternalWrite_RelayedRepositoryOutsideTheRunIsUnlinked(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	ctx := context.Background()
	ownID := registerRepo(t, stores, "octo", "repo", "1")
	registerRepo(t, stores, "octo", "other", "2")
	if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, info.OrgID, domain.ConversationWorktree{
		ConversationID: info.ConversationID, RepositoryID: ownID, Ref: "refs/heads/main", Path: "/runs/octo/repo/main",
	}); err != nil {
		t.Fatalf("seed the run's worktree: %v", err)
	}
	record := func(key, target, repositoryID, nodeID string) domain.ExternalAction {
		t.Helper()
		act := domain.ExternalAction{Provider: domain.ProviderGitHub, Action: domain.ActionLabelAdded, Target: target, Credential: domain.CredentialGitHubApp, DedupKey: key}
		id := domain.GitHubIdentity{RepositoryID: repositoryID, NodeID: nodeID}
		if nodeID != "" {
			id.Kind = domain.EntityKindPullRequest
		}
		recordExternalWrite(ctx, stores, info, nil, &act, domain.EntityContainer{}, id)
		for _, a := range listExternalActions(t, stores) {
			if a.DedupKey == key {
				return a
			}
		}
		t.Fatalf("no row for %s", key)
		return domain.ExternalAction{}
	}
	if a := record("own repo", "octo/repo", "1", ""); a.RepositoryID != ownID {
		t.Errorf("an act in the run's repository references %q, want %s", a.RepositoryID, ownID)
	}
	if a := record("other repo", "octo/other", "2", ""); a.RepositoryID != "" {
		t.Errorf("an act relayed under another repository's id references %q, want none", a.RepositoryID)
	}
	if a := record("other pr", "octo/other#4", "2", "PR_other4"); a.EntityID != "" || a.RepositoryID != "" {
		t.Errorf("an act on a pull request in another repository references (%q, %q), want neither", a.EntityID, a.RepositoryID)
	}
	if e, err := stores.Entities.GetBySourceSystem(ctx, info.OrgID, "github", testScope("github"), "octo/other#4"); err != nil || e != nil {
		t.Errorf("octo/other#4 = %+v, %v; want no entity", e, err)
	}
}
