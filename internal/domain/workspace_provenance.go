package domain

// WorkspaceProvenance is how the run tree an engagement is about to work in
// came to be. It is the claim path's knowledge and nothing else's: the loop
// driving the conversation sees a directory and cannot tell whether it is the
// one the last engagement left behind or a reconstruction of it.
//
// It travels because one thing the agent is told depends on it — whether its
// remembered work is actually on disk. A warm tree carries that work; a
// restored or newly built one carries only what the last snapshot held.
type WorkspaceProvenance string

const (
	// WorkspaceProvenanceUnknown is the zero value, and means the caller
	// neither builds nor restores workspaces (a fixture, a surface whose
	// tree it does not own). It is deliberately indistinguishable from warm
	// at the point of use: a caller that cannot say a restore happened must
	// not have one asserted on its behalf.
	WorkspaceProvenanceUnknown WorkspaceProvenance = ""
	// WorkspaceProvenanceWarm is the tree the previous engagement left on
	// this host, reused untouched. Whatever that engagement did is present,
	// including uncommitted and untracked files.
	WorkspaceProvenanceWarm WorkspaceProvenance = "warm"
	// WorkspaceProvenanceRehydrated is a tree rebuilt from the durable
	// snapshot because the warm copy was gone — another executor, a wiped
	// run root, a startup sweep. Everything up to the snapshot point is
	// present; work done after it is not.
	WorkspaceProvenanceRehydrated WorkspaceProvenance = "rehydrated"
	// WorkspaceProvenanceFresh is a tree built from scratch: a first claim's
	// clone, or a rebuild with no snapshot to restore from.
	WorkspaceProvenanceFresh WorkspaceProvenance = "fresh"
)

// Restored reports whether the tree is a reconstruction rather than the one
// the last engagement was working in — the single question the agent-facing
// notice turns on.
func (p WorkspaceProvenance) Restored() bool {
	return p == WorkspaceProvenanceRehydrated || p == WorkspaceProvenanceFresh
}
