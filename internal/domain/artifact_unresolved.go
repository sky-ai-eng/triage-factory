package domain

// Unresolved-artifact derivation (TFAC-492). A blueprint run no longer parks in
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

// UnresolvedArtifactCounts returns how many draft pull requests and ready
// (finalized) pending reviews in arts are still awaiting resolution. Same
// predicates as HasUnresolvedArtifacts, broken out per kind for the run
// projection (unresolved_pr_count / unresolved_review_count). A set is plural in
// each kind (multiple draft PRs / reviews per run), so this counts every match
// rather than just the first.
func UnresolvedArtifactCounts(arts []Artifact) (prCount, reviewCount int) {
	for i := range arts {
		switch {
		case arts[i].Kind == ArtifactKindPullRequest && arts[i].State == ArtifactStatePRDraft:
			prCount++
		case arts[i].Kind == ArtifactKindReview && arts[i].State == ArtifactStateReviewPending:
			// A pending review only counts once it's finalized for approval — the
			// ready sentinel (ReviewEvent) is set. A started-but-not-submitted
			// review has nothing to approve yet, mirroring FirstReadyReview.
			if d, err := ParseReviewArtifactDetails(arts[i].DetailsJSON); err == nil && d.ReviewEvent != "" {
				reviewCount++
			}
		}
	}
	return prCount, reviewCount
}
