package domain

import "time"

// GitHub's answers to a pull request's mergeable field. UNKNOWN means GitHub
// has not finished computing it, which is no answer: it is never stored.
const (
	MergeableMergeable   = "MERGEABLE"
	MergeableConflicting = "CONFLICTING"
	MergeableUnknown     = "UNKNOWN"
)

// PRMergeability is a pull request's last known mergeability: the answer
// columns of one pull_request_mergeability row, which the probe worker is the
// only writer of. The row's path columns are PRPaths.
type PRMergeability struct {
	EntityID string
	OrgID    string
	// Mergeable is the last known answer, MERGEABLE or CONFLICTING. Empty
	// while no read has answered: the row was created by a probe whose
	// retries all answered UNKNOWN.
	Mergeable string
	// MergeableHeadSHA is the pull request's head (headRefOid) at the last
	// read that settled a probe: a known answer, or the UNKNOWN that ended
	// its retries. It is the head the answer was read at only when that read
	// answered.
	MergeableHeadSHA string
	// MergeableBaseRef is the pull request's base branch (baseRefName) at
	// that same read; empty on a row whose reads predate the column.
	MergeableBaseRef string
	// CheckedAt is when that read was made; nil when it never was.
	CheckedAt *time.Time
}

// The reasons a mergeability probe is admitted for: the closed vocabulary of
// pr_mergeability_probes.reason. A probe that collapses into one already
// unsettled for its pull request keeps the first reason.
const (
	// MergeabilityProbeNew is a pull request whose mergeability was never
	// read: it has no row, or only the row its changed paths were written to.
	MergeabilityProbeNew = "new"
	// MergeabilityProbeHeadMoved is a pull request whose head is not the one
	// its row was last read at.
	MergeabilityProbeHeadMoved = "head_moved"
	// MergeabilityProbeBaseChanged is a pull request whose base branch is not
	// the one its row was last read against: it was retargeted.
	MergeabilityProbeBaseChanged = "base_changed"
	// MergeabilityProbeUnknownRetry is the re-read of a probe GitHub answered
	// UNKNOWN.
	MergeabilityProbeUnknownRetry = "unknown_retry"
	// MergeabilityProbeSweep is a pull request whose row was last read longer
	// than the sweep interval ago, or never.
	MergeabilityProbeSweep = "sweep"
	// MergeabilityProbeBaseMoved is a pull request whose base branch moved
	// and whose changed paths overlap what the move changed, or whose paths
	// TF does not hold in full.
	MergeabilityProbeBaseMoved = "base_moved"
	// MergeabilityProbeBaseReset is a pull request on a base branch TF cannot
	// tell the move of: the branch is newly seen, its history was rewritten,
	// its previous tip is gone, or the move changed too many files to list.
	MergeabilityProbeBaseReset = "base_reset"
)

// MergeabilityProbeReasons is the vocabulary above, which an admission is
// validated against.
var MergeabilityProbeReasons = []string{
	MergeabilityProbeNew,
	MergeabilityProbeHeadMoved,
	MergeabilityProbeBaseChanged,
	MergeabilityProbeUnknownRetry,
	MergeabilityProbeSweep,
	MergeabilityProbeBaseMoved,
	MergeabilityProbeBaseReset,
}

// GitHub's PatchStatus values: how a pull request, or a move of a branch,
// changed one file. A compare's file statuses are mapped onto these, so both
// sides of the overlap rule speak one vocabulary.
const (
	PathAdded    = "ADDED"
	PathDeleted  = "DELETED"
	PathRenamed  = "RENAMED"
	PathCopied   = "COPIED"
	PathModified = "MODIFIED"
	PathChanged  = "CHANGED"
)

// ChangedPath is one file a pull request or a branch move changed: its path,
// how it changed (a PatchStatus value), and for RENAMED and COPIED the path it
// came from.
type ChangedPath struct {
	Path     string `json:"path"`
	Change   string `json:"change"`
	Previous string `json:"previous,omitempty"`
}

// PRPaths is a pull request's changed paths as the base-branch watch last
// read them: the path columns of its pull_request_mergeability row, which the
// watch is the only writer of.
type PRPaths struct {
	EntityID string
	OrgID    string
	// Paths is nil when they were never read.
	Paths []ChangedPath
	// HeadSHA and BaseRef are the pull request's head (headRefOid) and base
	// branch (baseRefName) the list was read at, since the list is the
	// difference between the two; empty when it was never read.
	HeadSHA string
	BaseRef string
	// Complete is true when Paths holds every one of the pull request's
	// changed files.
	Complete bool
}

// BranchRef names one branch of one repository registry row. Branch is the
// name without refs/heads/.
type BranchRef struct {
	RepositoryID string
	Branch       string
}

// BranchTip is the last tip of a base branch the branch watch processed: one
// branch_tips row, written only by the watch.
type BranchTip struct {
	RepositoryID string
	Branch       string
	OrgID        string
	// TipSHA is the tip whose move, if any, has had its probes admitted.
	TipSHA string
	// ETag is the ref read's from its last 200; empty when it sent none.
	ETag string
	// ObservedAt is when the tip was last checked, a 304 included.
	ObservedAt time.Time
}

// Ref is the branch the tip is of.
func (b BranchTip) Ref() BranchRef {
	return BranchRef{RepositoryID: b.RepositoryID, Branch: b.Branch}
}
