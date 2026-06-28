package domain

// Unresolved-artifact derivation. A blueprint run no longer parks in
// pending_approval while a human approves a queued draft PR / pending review;
// instead the approval state is *derived* from the run's (blueprint's) artifact
// set. An artifact is "unresolved" when it still awaits a human verdict:
//
//   - a draft pull_request (state=draft) — FirstDraftPullRequest's predicate, or
//   - a finalized pending review (state=pending AND the ready sentinel
//     ReviewEvent is set) — FirstReadyReview's predicate.
//
// These helpers share those exact predicates so every consumer (board column,
// run projection, terminal task-close gate) agrees on what "unresolved" means.

// HasUnresolvedArtifacts reports whether arts contains at least one artifact
// still awaiting human resolution (a draft PR or a ready pending review). This
// is the derived signal that replaces the stored pending_approval run status: a
// task surfaces in the approval column whenever its artifact set has ≥1
// unresolved item, regardless of whether its run is live or terminal.
func HasUnresolvedArtifacts(arts []Artifact) bool {
	return FirstDraftPullRequest(arts) != nil || FirstReadyReview(arts) != nil
}

// UnresolvedArtifactIDs returns the ids of every unresolved approvable artifact
// in arts — every draft pull request plus every ready (finalized) pending review.
// Grouped by kind: all draft-PR ids first (in slice order), then all ready-review
// ids (in slice order). Same predicates as HasUnresolvedArtifacts /
// UnresolvedArtifactCounts, projected to ids for the run wire shape
// (pending_artifact_ids): the set of artifacts a human can still approve or
// dismiss. Always non-nil (empty slice when none) so the JSON field is [] rather
// than null.
func UnresolvedArtifactIDs(arts []Artifact) []string {
	ids := []string{}
	for _, a := range AllDraftPullRequests(arts) {
		ids = append(ids, a.ID)
	}
	for _, a := range AllReadyReviews(arts) {
		ids = append(ids, a.ID)
	}
	return ids
}

// UnresolvedArtifactCounts returns how many draft pull requests and ready
// (finalized) pending reviews in arts are still awaiting resolution. Same
// predicates as HasUnresolvedArtifacts, broken out per kind for the run
// projection (unresolved_pr_count / unresolved_review_count). A set is plural in
// each kind (multiple draft PRs / reviews per run), so this counts every match
// rather than just the first.
func UnresolvedArtifactCounts(arts []Artifact) (prCount, reviewCount int) {
	// Go through the same single-artifact predicates FirstDraftPullRequest /
	// FirstReadyReview use, so the count can't drift from the bool derivation if
	// either predicate changes.
	for i := range arts {
		switch {
		case isDraftPullRequest(arts[i]):
			prCount++
		case isReadyReview(arts[i]):
			reviewCount++
		}
	}
	return prCount, reviewCount
}
