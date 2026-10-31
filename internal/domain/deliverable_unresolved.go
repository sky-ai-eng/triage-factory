package domain

// Unresolved-deliverable derivation. A blueprint run never parks while a human
// approves a queued draft PR / pending review; the approval state is *derived*
// from the blueprint run's deliverable set instead. A deliverable is
// "unresolved" when it still awaits a human verdict:
//
//   - a draft pull_request (state=draft) — FirstDraftPullRequest's predicate, or
//   - a finalized pending review (state=pending AND the ready sentinel
//     ReviewEvent is set) — FirstReadyReview's predicate.
//
// These helpers share those exact predicates so every consumer agrees on what
// "unresolved" means: the attention filter (and its SQL twin in both dialects),
// the conversation projection's pending-deliverable fields, and the terminal
// task-close gate.

// HasUnresolvedDeliverables reports whether arts contains at least one deliverable
// still awaiting human resolution (a draft PR or a ready pending review). The
// signal is derived, never stored: ≥1 unresolved item is what keeps a task
// open and shows it as owing a human something, regardless of whether its
// conversation is live or terminal. It moves no board column.
func HasUnresolvedDeliverables(arts []Deliverable) bool {
	return FirstDraftPullRequest(arts) != nil || FirstReadyReview(arts) != nil
}

// UnresolvedDeliverableIDs returns the ids of every unresolved approvable deliverable
// in arts — every draft pull request plus every ready (finalized) pending review.
// Grouped by kind: all draft-PR ids first (in slice order), then all ready-review
// ids (in slice order). Same predicates as HasUnresolvedDeliverables /
// UnresolvedDeliverableCounts, projected to ids for the run wire shape
// (pending_deliverable_ids): the set of deliverables a human can still approve or
// dismiss. Always non-nil (empty slice when none) so the JSON field is [] rather
// than null.
func UnresolvedDeliverableIDs(arts []Deliverable) []string {
	ids := []string{}
	for _, a := range AllDraftPullRequests(arts) {
		ids = append(ids, a.ID)
	}
	for _, a := range AllReadyReviews(arts) {
		ids = append(ids, a.ID)
	}
	return ids
}

// DeliverableCountsByKind is how many deliverables of each kind a conversation
// produced, keyed by the kind string with absent kinds omitted. It is what the
// board card's footer strip states — what the run WROTE, by kind — and it is
// taken off the same deliverable set as UnresolvedDeliverableCounts so the strip's
// totals and its warm marks can never disagree.
func DeliverableCountsByKind(arts []Deliverable) map[string]int {
	out := map[string]int{}
	for _, a := range arts {
		out[a.Kind]++
	}
	return out
}

// UnresolvedDeliverableCounts returns how many draft pull requests and ready
// (finalized) pending reviews in arts are still awaiting resolution. Same
// predicates as HasUnresolvedDeliverables, broken out per kind for the run
// projection (unresolved_pr_count / unresolved_review_count). A set is plural in
// each kind (multiple draft PRs / reviews per conversation), so this counts every match
// rather than just the first.
func UnresolvedDeliverableCounts(arts []Deliverable) (prCount, reviewCount int) {
	// Go through the same single-deliverable predicates FirstDraftPullRequest /
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
