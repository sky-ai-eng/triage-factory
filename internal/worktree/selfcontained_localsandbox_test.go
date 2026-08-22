package worktree

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSelfContainedRunTrees_FollowsTheSandbox pins D6's decision: the split
// is about whether a SANDBOX holds the run tree, not about which mode the
// process is in. A local run with the mount namespace on sees the state root
// masked, so a linked worktree's .git pointer into the shared bare dangles
// there exactly as it does inside multi's jail — and binding the bares in
// instead would hand the run every repo the org has cloned, read-write, with
// concurrent runs free to corrupt each other's refs.
func TestSelfContainedRunTrees_FollowsTheSandbox(t *testing.T) {
	cases := []struct {
		name    string
		mode    runmode.Mode
		sandbox bool
		want    bool
	}{
		{name: "local unsandboxed keeps zero-copy worktrees", mode: runmode.ModeLocal, sandbox: false, want: false},
		{name: "local sandboxed clones", mode: runmode.ModeLocal, sandbox: true, want: true},
		{name: "multi always clones", mode: runmode.ModeMulti, sandbox: false, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runmode.SetForTest(t, c.mode)
			runmode.SetLocalSandboxForTest(t, c.sandbox)
			if got := selfContainedRunTrees(); got != c.want {
				t.Errorf("selfContainedRunTrees() = %t, want %t", got, c.want)
			}
		})
	}
}
