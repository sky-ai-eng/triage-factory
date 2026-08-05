package domain

// WorkspaceProvenance is how the run tree an engagement is about to work in
// came to be. It is the claim path's knowledge and nothing else's: the loop
// driving the conversation sees a directory and cannot tell whether it is the
// one the last engagement left behind or something built where it used to be.
//
// It travels because one thing the agent is told depends on it — how much of
// its remembered work is actually on disk. The three answers are genuinely
// different, and the distinction between the two rebuilt ones matters as much
// as the distinction from warm: a snapshot carries uncommitted and untracked
// files, and a tree built without one carries nothing that never left the
// host.
type WorkspaceProvenance string

const (
	// WorkspaceProvenanceUnknown is the zero value, and means the caller
	// neither builds nor restores workspaces (a fixture, a surface whose
	// tree it does not own). It is deliberately indistinguishable from warm
	// at the point of use: a caller that cannot say a rebuild happened must
	// not have one asserted on its behalf.
	WorkspaceProvenanceUnknown WorkspaceProvenance = ""
	// WorkspaceProvenanceWarm is the tree the previous engagement left on
	// this host, reused untouched. Whatever that engagement did is present,
	// including uncommitted and untracked files.
	WorkspaceProvenanceWarm WorkspaceProvenance = "warm"
	// WorkspaceProvenanceRehydrated is a tree rebuilt from the durable
	// snapshot because the warm copy was gone — another executor, a wiped
	// run root, a startup sweep. Snapshots are taken at graceful dormancy
	// points, so everything up to that point is present, uncommitted and
	// untracked files included; work done after it is not.
	WorkspaceProvenanceRehydrated WorkspaceProvenance = "rehydrated"
	// WorkspaceProvenanceFresh is a tree the source setup built from
	// nothing — a first claim's clone and checkout. No snapshot is involved
	// and none is consulted, so it holds exactly what the remote holds:
	// anything a previous engagement pushed, and none of what it only ever
	// had locally.
	WorkspaceProvenanceFresh WorkspaceProvenance = "fresh"
)

// Rebuilt reports whether the tree is something built where the last
// engagement's workspace used to be, rather than that workspace itself — the
// question that decides whether the agent is told anything at all about it.
// What it is then told depends on which rebuild it was.
func (p WorkspaceProvenance) Rebuilt() bool {
	return p == WorkspaceProvenanceRehydrated || p == WorkspaceProvenanceFresh
}
