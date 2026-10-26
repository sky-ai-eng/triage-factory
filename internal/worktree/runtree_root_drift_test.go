package worktree

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
//
// Both modes, because the two derive the base per mode: the temp dir locally,
// the state root in multi.
func TestRunRoot_MatchesBrokerTrustedRoot(t *testing.T) {
	for _, mode := range []runmode.Mode{runmode.ModeLocal, runmode.ModeMulti} {
		t.Run(string(mode), func(t *testing.T) {
			runmode.SetForTest(t, mode)
			t.Setenv("TMPDIR", t.TempDir())
			paths.SetForTest(t, t.TempDir())
			for _, rootKey := range []string{"run-1", "00000000-0000-0000-0000-0000000000aa", "itest-abc"} {
				if got, want := RunRoot(rootKey), sandbox.RunTreeRoot(rootKey); got != want {
					t.Errorf("RunRoot(%q) = %q, want sandbox.RunTreeRoot %q", rootKey, got, want)
				}
			}
		})
	}
}
