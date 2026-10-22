package agenthost

import (
	"context"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestUpsertArtifact_KeysABranchOnItsTargetNotTheKeyItArrivesWith covers the
// relayed write: on an executor the sidecar builds a branch's key, and the
// runtime must not file the push under a repository the key names, or fail on
// a key that names none. The branch is keyed on the row its target names, and
// a target no row has is refused while the push keeps its ledger row.
func TestUpsertArtifact_KeysABranchOnItsTargetNotTheKeyItArrivesWith(t *testing.T) {
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

			for i, sentKey := range []string{
				domain.BranchDedupKey(otherID, ref), // another repository in the org
				"git:branch:not-a-uuid:" + ref,
			} {
				branch, _ := domain.NewBranchArtifact("octo/repo", ref, []string{"abc123", "def456"}[i], i == 0)
				branch.DedupKey = sentKey
				stored, err := rt.UpsertArtifact(ctx, branch)
				if err != nil {
					t.Fatalf("UpsertArtifact with key %q: %v", sentKey, err)
				}
				if want := domain.BranchDedupKey(repoID, ref); stored.DedupKey != want {
					t.Errorf("sent %q: stored key = %q, want %q", sentKey, stored.DedupKey, want)
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

			ghost, _ := domain.NewBranchArtifact("octo/ghost", ref, "abc123", true)
			ghost.DedupKey = domain.BranchDedupKey(repoID, ref)
			if _, err := rt.UpsertArtifact(ctx, ghost); !errors.Is(err, errNoRegistryRow) {
				t.Errorf("a target no row has: err = %v, want errNoRegistryRow", err)
			}
			var ghostRow *domain.ExternalAction
			for _, a := range listExternalActions(t, stores) {
				if a.Target == "octo/ghost" {
					ghostRow = &a
				}
			}
			if ghostRow == nil || ghostRow.RepositoryID != "" {
				t.Errorf("a target no row has: push row = %+v, want one naming no repository", ghostRow)
			}
		})
	}
}
