package worktree

import "github.com/sky-ai-eng/triage-factory/internal/runmode"

// selfContainedRunTrees reports whether a run's tree must be materialized as
// a STANDALONE clone rather than a zero-copy linked worktree.
//
// A linked worktree's .git is a pointer into the shared bare under
// <StateRoot>/repos. That is free and correct as long as the agent can see
// the bare — and it cannot, in either sandbox. Multi's gVisor jail bind-mounts
// the run tree and nothing else; the local bubblewrap plan masks the state
// root for the same reason it masks the home directory. Binding the bares in
// would give the run every repo the org has ever cloned, read-write, with
// concurrent runs free to corrupt each other's refs — so the tree carries its
// own objects instead.
//
// The cost is clone time and disk on a path multi already exercises daily,
// and the escape hatch is the same one that turns the sandbox off:
// TF_LOCAL_SANDBOX=off restores local's zero-copy worktrees.
func selfContainedRunTrees() bool {
	return runmode.Current() == runmode.ModeMulti || runmode.LocalSandboxEnabled()
}
