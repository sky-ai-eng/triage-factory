package domain

import "time"

// GitHub's answers to a pull request's mergeable field. UNKNOWN means GitHub
// has not finished computing it, which is no answer: it is never stored.
const (
	MergeableMergeable   = "MERGEABLE"
	MergeableConflicting = "CONFLICTING"
	MergeableUnknown     = "UNKNOWN"
)

// PRMergeability is a pull request's last known mergeability: one
// pull_request_mergeability row, written only by the probe worker.
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
	// CheckedAt is when that read was made; nil when it never was.
	CheckedAt *time.Time
}

// The reasons a mergeability probe is admitted for: the closed vocabulary of
// pr_mergeability_probes.reason. A probe that collapses into one already
// unsettled for its pull request keeps the first reason.
const (
	// MergeabilityProbeNew is a pull request with no mergeability row.
	MergeabilityProbeNew = "new"
	// MergeabilityProbeHeadMoved is a pull request whose head is not the one
	// its row was last read at.
	MergeabilityProbeHeadMoved = "head_moved"
	// MergeabilityProbeUnknownRetry is the re-read of a probe GitHub answered
	// UNKNOWN.
	MergeabilityProbeUnknownRetry = "unknown_retry"
	// MergeabilityProbeSweep is a pull request whose row was last read longer
	// than the sweep interval ago, or never.
	MergeabilityProbeSweep = "sweep"
)

// MergeabilityProbeReasons is the vocabulary above, which an admission is
// validated against.
var MergeabilityProbeReasons = []string{
	MergeabilityProbeNew,
	MergeabilityProbeHeadMoved,
	MergeabilityProbeUnknownRetry,
	MergeabilityProbeSweep,
}
