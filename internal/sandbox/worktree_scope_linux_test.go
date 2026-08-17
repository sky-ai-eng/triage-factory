package sandbox

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// TestWorktreeScope_AcceptsRunAndBlueprintKeys pins the resumed-run fix: a run's
// ephemeral tree lives under its run id on first launch and under its memory
// namespace (blueprint run id) after a cold rehydrate rebuilds it, so
// worktreeScope must accept EITHER of the run's own keys. It must still reject a
// THIRD run's tree, and — on a fresh deploy with no orgs subtree — reject with a
// clean message rather than the bare lstat of the absent orgs dir.
func TestWorktreeScope_AcceptsRunAndBlueprintKeys(t *testing.T) {
	paths.SetForTest(t, t.TempDir()) // state root with no orgs/ subtree

	const (
		conversationID = "run-aaaaaaaa"
		blueprint      = "bp-bbbbbbbb"
		other          = "run-cccccccc"
	)
	runTree := ensureRunTreeFixture(t, conversationID)
	bpTree := ensureRunTreeFixture(t, blueprint)
	otherTree := ensureRunTreeFixture(t, other)

	t.Run("run-id-keyed tree (first launch)", func(t *testing.T) {
		_, hasScope, err := worktreeScope(conversationID, blueprint, runTree)
		if err != nil || hasScope {
			t.Fatalf("worktreeScope(run-id tree) = (hasScope=%v, %v), want (false, nil)", hasScope, err)
		}
	})

	t.Run("blueprint-keyed tree (cold rehydrate)", func(t *testing.T) {
		// The regression: worktree == RunTreeRoot(memoryNamespace) while conversationID
		// differs. A resumed run's re-keyed tree must be accepted.
		_, hasScope, err := worktreeScope(conversationID, blueprint, bpTree)
		if err != nil || hasScope {
			t.Fatalf("worktreeScope(blueprint tree) = (hasScope=%v, %v), want (false, nil)", hasScope, err)
		}
	})

	t.Run("a third run's tree is rejected cleanly", func(t *testing.T) {
		_, _, err := worktreeScope(conversationID, blueprint, otherTree)
		if err == nil {
			t.Fatal("worktreeScope accepted a tree keyed by neither the run nor its blueprint")
		}
		if strings.Contains(err.Error(), "lstat") || strings.Contains(err.Error(), "no such file") {
			t.Errorf("rejection surfaced the bare lstat of the missing orgs dir: %v", err)
		}
	})

	t.Run("empty memory namespace falls back to the run id alone", func(t *testing.T) {
		if _, _, err := worktreeScope(conversationID, "", runTree); err != nil {
			t.Fatalf("worktreeScope(conversationID, \"\", run tree) = %v, want nil", err)
		}
		if _, _, err := worktreeScope(conversationID, "", bpTree); err == nil {
			t.Fatal("worktreeScope accepted the blueprint tree with no memory namespace supplied")
		}
	})
}
