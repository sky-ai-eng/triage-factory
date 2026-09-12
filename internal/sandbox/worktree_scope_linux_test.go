package sandbox

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// TestWorktreeScope_AcceptsRunAndWorkspaceKeys pins the resumed-run fix: a run's
// ephemeral tree lives under its run id on first launch and under its workspace
// key (the task id) after a cold rehydrate rebuilds it, so worktreeScope must
// accept EITHER of the run's own keys. It must still reject a THIRD run's tree,
// and — on a fresh deploy with no orgs subtree — reject with a clean message
// rather than the bare lstat of the absent orgs dir.
func TestWorktreeScope_AcceptsRunAndWorkspaceKeys(t *testing.T) {
	paths.SetForTest(t, t.TempDir()) // state root with no orgs/ subtree

	const (
		conversationID = "run-aaaaaaaa"
		wsKey          = "task-bbbbbbbb"
		other          = "run-cccccccc"
	)
	runTree := ensureRunTreeFixture(t, conversationID)
	wsTree := ensureRunTreeFixture(t, wsKey)
	otherTree := ensureRunTreeFixture(t, other)

	t.Run("run-id-keyed tree (first launch)", func(t *testing.T) {
		_, hasScope, err := worktreeScope(conversationID, wsKey, runTree)
		if err != nil || hasScope {
			t.Fatalf("worktreeScope(run-id tree) = (hasScope=%v, %v), want (false, nil)", hasScope, err)
		}
	})

	t.Run("workspace-keyed tree (cold rehydrate)", func(t *testing.T) {
		// The regression: worktree == RunTreeRoot(workspaceKey) while conversationID
		// differs. A resumed run's re-keyed tree must be accepted.
		_, hasScope, err := worktreeScope(conversationID, wsKey, wsTree)
		if err != nil || hasScope {
			t.Fatalf("worktreeScope(workspace tree) = (hasScope=%v, %v), want (false, nil)", hasScope, err)
		}
	})

	t.Run("a third run's tree is rejected cleanly", func(t *testing.T) {
		_, _, err := worktreeScope(conversationID, wsKey, otherTree)
		if err == nil {
			t.Fatal("worktreeScope accepted a tree keyed by neither the run nor its workspace")
		}
		if strings.Contains(err.Error(), "lstat") || strings.Contains(err.Error(), "no such file") {
			t.Errorf("rejection surfaced the bare lstat of the missing orgs dir: %v", err)
		}
	})

	t.Run("empty workspace key falls back to the run id alone", func(t *testing.T) {
		if _, _, err := worktreeScope(conversationID, "", runTree); err != nil {
			t.Fatalf("worktreeScope(conversationID, \"\", run tree) = %v, want nil", err)
		}
		if _, _, err := worktreeScope(conversationID, "", wsTree); err == nil {
			t.Fatal("worktreeScope accepted the workspace tree with no workspace key supplied")
		}
	})
}
