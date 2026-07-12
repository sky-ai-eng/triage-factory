package worktree

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestRunRoot_MatchesBrokerTrustedRoot is the drift guard for the broker's
// worktree-confinement pinning: the broker (internal/sandbox) validates a
// delegated task run's Config.Worktree against sandbox.RunTreeRoot, which
// duplicates this package's runDir/runsDir literal rather than importing it
// (this package already imports internal/sandbox for ChownRunTree /
// RemoveRunTree, so the reverse import would cycle). A drift between the
// two would make every real delegated run's worktree fail the broker's
// launch validation — this test is what catches that instead of it
// surfacing as every sandboxed run rejected in production.
func TestRunRoot_MatchesBrokerTrustedRoot(t *testing.T) {
	for _, runID := range []string{"run-1", "00000000-0000-0000-0000-0000000000aa", "itest-abc"} {
		if got, want := RunRoot(runID), sandbox.RunTreeRoot(runID); got != want {
			t.Errorf("RunRoot(%q) = %q, want sandbox.RunTreeRoot %q", runID, got, want)
		}
	}
}
