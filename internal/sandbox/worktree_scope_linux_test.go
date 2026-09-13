package sandbox

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// TestWorktreeScope_AcceptsTheWorkspaceKeyedTree pins the tree shape the
// broker will accept: a run's ephemeral tree lives under its workspace key —
// the task id — on the first launch and after a cold rehydrate rebuilds it,
// and nothing builds one under a conversation id. So the workspace key is the
// only key that resolves. It must still reject another task's tree, and — on a
// fresh deploy with no orgs subtree — reject with a clean message rather than
// the bare lstat of the absent orgs dir.
func TestWorktreeScope_AcceptsTheWorkspaceKeyedTree(t *testing.T) {
	paths.SetForTest(t, t.TempDir()) // state root with no orgs/ subtree

	const (
		conversationID = "run-aaaaaaaa"
		wsKey          = "task-bbbbbbbb"
		other          = "task-cccccccc"
	)
	conversationTree := ensureRunTreeFixture(t, conversationID)
	wsTree := ensureRunTreeFixture(t, wsKey)
	otherTree := ensureRunTreeFixture(t, other)

	t.Run("the workspace-keyed tree", func(t *testing.T) {
		_, hasScope, err := worktreeScope(wsKey, wsTree)
		if err != nil || hasScope {
			t.Fatalf("worktreeScope(workspace tree) = (hasScope=%v, %v), want (false, nil)", hasScope, err)
		}
	})

	t.Run("another task's tree is rejected cleanly", func(t *testing.T) {
		_, _, err := worktreeScope(wsKey, otherTree)
		if err == nil {
			t.Fatal("worktreeScope accepted a tree keyed by something other than this run's workspace")
		}
		if strings.Contains(err.Error(), "lstat") || strings.Contains(err.Error(), "no such file") {
			t.Errorf("rejection surfaced the bare lstat of the missing orgs dir: %v", err)
		}
	})

	t.Run("a conversation-keyed tree is not a shape the broker knows", func(t *testing.T) {
		// Nothing builds a tree under a conversation id, so one presented as a
		// worktree is indistinguishable from any other stranger's path.
		if _, _, err := worktreeScope(wsKey, conversationTree); err == nil {
			t.Fatal("worktreeScope accepted a conversation-keyed tree")
		}
	})

	t.Run("no workspace key accepts no ephemeral tree at all", func(t *testing.T) {
		if _, _, err := worktreeScope("", wsTree); err == nil {
			t.Fatal("worktreeScope accepted an ephemeral tree with no workspace key supplied")
		}
	})
}
