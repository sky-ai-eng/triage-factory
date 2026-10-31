package agenthost

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestUpsertDeliverable_KeysABranchOnTheRepositoryItsIDNames covers the
// relayed write: on an executor the sidecar builds a branch's key, and the
// runtime must not file the push under a repository the key names, or fail on
// a key that names none. The branch is keyed on the row GitHub's repository id
// names, under that row's name, whatever its target spells; a push whose
// identity read failed is refused while the push keeps its ledger row.
func TestUpsertDeliverable_KeysABranchOnTheRepositoryItsIDNames(t *testing.T) {
	const ref = "refs/heads/feat"
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			stores, info := newCaptureStores(t, eventTriggered)
			rt := newDirectRuntime(stores, info)
			ctx := context.Background()
			repoID := registerRepo(t, stores, "octo", "repo", "1")
			otherID := registerRepo(t, stores, "octo", "other", "2")
			id := domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo"}

			for i, sentKey := range []string{
				domain.BranchDedupKey(otherID, ref), // another repository in the org
				"git:branch:not-a-uuid:" + ref,
			} {
				// The second spells the repository by a name it has had: the id,
				// not the name, decides the row.
				branch, _ := domain.NewBranchDeliverable([]string{"octo/repo", "octo/old-repo"}[i], ref, []string{"abc123", "def456"}[i], i == 0)
				branch.DedupKey = sentKey
				stored, err := rt.UpsertDeliverable(ctx, branch, id)
				if err != nil {
					t.Fatalf("UpsertDeliverable with key %q: %v", sentKey, err)
				}
				if want := domain.BranchDedupKey(repoID, ref); stored.DedupKey != want || stored.Target != "octo/repo" {
					t.Errorf("sent %q: stored key %q target %q, want %q under octo/repo", sentKey, stored.DedupKey, stored.Target, want)
				}
			}
			for _, a := range listExternalActions(t, stores) {
				if a.RepositoryID != repoID {
					t.Errorf("push row %s names repository %q, want octo/repo's %s", a.DedupKey, a.RepositoryID, repoID)
				}
			}
			if got := len(listExternalActions(t, stores)); got != 2 {
				t.Errorf("push rows = %d, want 2", got)
			}

			ghost, _ := domain.NewBranchDeliverable("octo/ghost", ref, "abc123", true)
			ghost.DedupKey = domain.BranchDedupKey(repoID, ref)
			if _, err := rt.UpsertDeliverable(ctx, ghost, domain.GitHubIdentity{}); !errors.Is(err, errNoRegistryRow) {
				t.Errorf("a push whose identity read failed: err = %v, want errNoRegistryRow", err)
			}
			var ghostRow *domain.ExternalAction
			for _, a := range listExternalActions(t, stores) {
				if a.Target == "octo/ghost" {
					ghostRow = &a
				}
			}
			if ghostRow == nil || ghostRow.RepositoryID != "" {
				t.Errorf("a push whose identity read failed: push row = %+v, want one naming no repository", ghostRow)
			}
		})
	}
}

// TestUpsertDeliverable_RelayedRepositoryOutsideTheRunIsUnlinked: in multi
// mode the repository id a branch arrives with was read by the run's sidecar,
// so it keys the branch only when it names a repository the run works in — a
// worktree of the run's, or its task's own repository. Any other id records
// the push unlinked, so a sidecar cannot file a push under a repository of its
// choosing.
func TestUpsertDeliverable_RelayedRepositoryOutsideTheRunIsUnlinked(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	runmode.SetForTest(t, runmode.ModeMulti)
	rt := newDirectRuntime(stores, info)
	ctx := context.Background()
	ownID := registerRepo(t, stores, "octo", "repo", "1")
	registerRepo(t, stores, "octo", "other", "2")
	if _, _, err := stores.ConversationWorktrees.InsertSystem(ctx, info.OrgID, domain.ConversationWorktree{
		ConversationID: info.ConversationID, RepositoryID: ownID, Ref: "refs/heads/main", Path: "/runs/octo/repo/main",
	}); err != nil {
		t.Fatalf("seed the run's worktree: %v", err)
	}

	own, _ := domain.NewBranchDeliverable("octo/repo", "refs/heads/feat", "abc123", true)
	if stored, err := rt.UpsertDeliverable(ctx, own, domain.GitHubIdentity{RepositoryID: "1", Repository: "octo/repo"}); err != nil ||
		stored.DedupKey != domain.BranchDedupKey(ownID, "refs/heads/feat") {
		t.Fatalf("the run's own repository: stored %+v err=%v, want it keyed on %s", stored, err, ownID)
	}
	other, _ := domain.NewBranchDeliverable("octo/repo", "refs/heads/feat", "def456", false)
	if _, err := rt.UpsertDeliverable(ctx, other, domain.GitHubIdentity{RepositoryID: "2", Repository: "octo/other"}); !errors.Is(err, errNoRegistryRow) {
		t.Fatalf("a repository outside the run: err = %v, want errNoRegistryRow", err)
	}
	for _, a := range listExternalActions(t, stores) {
		if domain.ParseBranchDeliverableSHA(a.DetailJSON) == "def456" && a.RepositoryID != "" {
			t.Errorf("the push relayed under another repository's id names repository %q, want none", a.RepositoryID)
		}
	}
}
