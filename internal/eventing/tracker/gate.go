package tracker

import (
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// staleRefreshFloor caps how long an open PR can sit on a skipped
// refresh before we force a full fetch regardless of the other gate
// signals. Bounds the worst case for events that don't bump
// PullRequest.updatedAt — most importantly workflow re-runs that
// add a brand-new check_run on an unchanged head SHA. Without this
// floor, once the prior run is terminal and the PR is otherwise
// quiet, ci_failed / ci_passed for the re-run would never surface
// until some unrelated PR mutation eventually triggered a refresh.
//
// 10 minutes trades ~10x cost reduction at 1-minute polling for at
// most a 10-minute lag on workflow-rerun CI events. Tunable knob if
// real users care about tighter freshness; the right answer at that
// point is webhooks.
const staleRefreshFloor = 10 * time.Minute

// shouldSkipRefresh reports whether a tracked open PR can keep its
// stored snapshot for this poll cycle (no follow-up RefreshPRs call
// needed). Skipping means we trust the stored snapshot through this
// cycle and emit no events for the entity.
//
// age is how long it's been since this entity was last fully refreshed
// (typically time.Since(*entity.LastPolledAt), with nil treated as
// "very stale"). The caller computes it; tests pass it directly.
//
// Why this is safe in the common case:
//
//   - Anything that changes the diff scope at the PR level (commits,
//     reviews, labels, comments, ready_for_review, mergeable, title,
//     base ref) bumps GitHub's PullRequest.updatedAt. If updatedAt
//     matches stored, none of those happened.
//   - Head SHA changes always bump updatedAt (a push is a PR-level
//     mutation), but we belt-and-brace it because a regression there
//     would silently drop new_commits events.
//
// Why the in-flight CI guard is necessary: check runs live on commits,
// not on the PR row. A check run completing does NOT bump
// PullRequest.updatedAt. So gating on updatedAt alone would lose
// ci_passed / ci_failed transitions. The guard forces a refresh on any
// stored snapshot where CI is still running.
//
// Why the CheckRuns!=nil guard is necessary: a freshly discovered
// entity has its snapshot seeded from the discovery fragment, which
// doesn't include check_runs (CheckRuns == nil signals "unknown prior
// state"). On its first poll-cycle pass through this gate, fresh and
// stored will have the same updatedAt + SHA, but the stored snapshot
// has no CI data. Skipping would leave it that way forever. Forcing
// the first refresh fills it in and the gate engages on subsequent
// cycles.
//
// Why the staleness floor is necessary: a workflow re-run on an
// unchanged head SHA can add a brand-new check_run after the prior
// run already reached "completed". updatedAt doesn't bump on the
// re-run's start OR completion; in-flight CI doesn't fire because
// the previously-stored snapshot had everything terminal. Without
// the floor, the gate would skip the new run forever — the failure
// or success of that re-run would never be emitted before merge.
// The floor caps the worst case at staleRefreshFloor.
func shouldSkipRefresh(stored, fresh domain.PRSnapshot, age time.Duration) bool {
	if age > staleRefreshFloor {
		return false
	}
	if stored.UpdatedAt == "" || fresh.UpdatedAt == "" {
		return false
	}
	if stored.UpdatedAt != fresh.UpdatedAt {
		return false
	}
	if stored.HeadSHA == "" || fresh.HeadSHA == "" {
		return false
	}
	if stored.HeadSHA != fresh.HeadSHA {
		return false
	}
	// A fresh body also fills a missing baseline on deployed snapshots.
	if fresh.BodyHash != "" && stored.BodyHash != fresh.BodyHash {
		return false
	}
	if stored.CheckRuns == nil {
		return false
	}
	if hasInFlightCI(stored) {
		return false
	}
	return true
}

// hasInFlightCI reports whether the snapshot contains any check_run
// in a non-terminal state. Only "completed" is terminal; any other
// status should be treated as still in flight until GitHub reports
// completion and populates conclusion.
func hasInFlightCI(snap domain.PRSnapshot) bool {
	for _, cr := range snap.CheckRuns {
		if cr.Status != "completed" {
			return true
		}
	}
	return false
}
