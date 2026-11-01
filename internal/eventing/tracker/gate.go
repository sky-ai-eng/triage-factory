package tracker

import (
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// staleRefreshFloor caps how long an open PR can sit on a skipped
// refresh before we force a full fetch regardless of the other gate
// signals. What it still covers is a check run that appears with no
// change to the pull request: a workflow re-run on an unchanged head
// SHA, after the prior run is terminal and changeRefreshWindow has
// passed. Neither the re-run's start nor its completion bumps
// PullRequest.updatedAt, so without this floor its ci_failed /
// ci_passed would never surface until some unrelated PR mutation
// eventually triggered a refresh.
//
// 10 minutes trades ~10x cost reduction at 1-minute polling for at
// most a 10-minute lag on workflow-rerun CI events. If real users need
// tighter freshness than that, the answer is webhooks.
const staleRefreshFloor = 10 * time.Minute

// changeRefreshWindow is how long after TF last saw a pull request
// change (PRSnapshot.ChangeSeenAt) the gate refreshes it every cycle,
// whatever its stored checks say. Check runs appear after the change
// that starts them: GitHub Actions creates them seconds after a push,
// third-party CI can take minutes. A refresh that lands in between
// stores no check runs, and a check run completing never bumps
// updatedAt, so without the window that pull request's CI result would
// wait for staleRefreshFloor.
const changeRefreshWindow = 5 * time.Minute

// mergeabilitySweepInterval is how long after a pull request's last
// mergeability read the poll admits a probe to read it again, whatever else
// did or did not move. A new head, a new base branch, and a base branch move
// that touches the pull request's files (the branch watch) are probed for when
// the poll sees them; the sweep catches the changes those miss, such as a
// rename the base made that git stops pairing after a later rewrite of the
// file.
const mergeabilitySweepInterval = 6 * time.Hour

// mergeabilityGiveUpRecheck is how long after GitHub answered UNKNOWN on every
// retry of a round of reads the poll admits a fresh round. GitHub computing
// mergeability that slowly is load on its side, which a pause can outlast,
// and the sweep would leave a conflict unseen for hours.
const mergeabilityGiveUpRecheck = 10 * time.Minute

// giveUpRecheck is the pause after the n-th give-up in a row before a fresh
// round: mergeabilityGiveUpRecheck, doubled for each give-up before the n-th,
// and never longer than mergeabilitySweepInterval, so a pull request GitHub
// keeps answering UNKNOWN for costs fewer reads the longer it goes on.
func giveUpRecheck(n int) time.Duration {
	d := mergeabilityGiveUpRecheck
	for i := 1; i < n && d < mergeabilitySweepInterval; i++ {
		d *= 2
	}
	return min(d, mergeabilitySweepInterval)
}

// shouldSkipRefresh reports whether a tracked open PR can keep its
// stored snapshot for this poll cycle (no follow-up RefreshPRs call
// needed). Skipping means we trust the stored snapshot through this
// cycle and emit no events for the entity.
//
// age is how long it's been since this entity was last fully refreshed
// (typically time.Since(*entity.LastPolledAt), with nil treated as
// "very stale"). sinceChange is how long it's been since TF last saw the
// pull request change (time.Since of the stored snapshot's ChangeSeenAt,
// with a missing or unparseable stamp treated as changeRefreshWindow,
// outside the window; a negative value, from a pod whose clock runs
// ahead, is inside it). The caller computes both; tests pass them
// directly.
//
// Why this is safe in the common case:
//
//   - Anything that changes the diff scope at the PR level (commits,
//     reviews, labels, comments, ready_for_review, title, base ref) bumps
//     GitHub's PullRequest.updatedAt. If updatedAt matches stored, none of
//     those happened.
//   - Head SHA changes always bump updatedAt (a push is a PR-level
//     mutation), but we belt-and-brace it because a regression there
//     would silently drop new_commits events.
//   - The check runs a change starts are the exception: GitHub creates
//     them after the change, so the refresh that saw the change can have
//     stored none, and they never bump updatedAt. For changeRefreshWindow
//     after TF last saw the pull request change, the gate refreshes it
//     every cycle before any other check, so the check runs created in
//     that time are read. That holds on the quiet-repository path too,
//     where a 304 listing feeds the stored snapshot back as fresh: that is
//     the path a settled pull request takes.
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
// run already reached "completed", with no change to the pull request
// to open the change window. updatedAt doesn't bump on the re-run's
// start OR completion; in-flight CI doesn't fire because the
// previously-stored snapshot had everything terminal. Without the
// floor, the gate would skip the new run forever — the failure or
// success of that re-run would never be emitted before merge. The
// floor caps the worst case at staleRefreshFloor.
func shouldSkipRefresh(stored, fresh domain.PRSnapshot, age, sinceChange time.Duration) bool {
	if sinceChange < changeRefreshWindow {
		return false
	}
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

// sinceChangeSeen is how long before now TF last saw the pull request change,
// read from the stored snapshot's ChangeSeenAt. A snapshot with no stamp
// (stored before the field existed, or seeded quietly) or an unparseable one
// gives changeRefreshWindow, outside the window, so it gates exactly as it did
// before the stamp existed. A stamp ahead of now gives a negative duration,
// inside the window.
func sinceChangeSeen(snap domain.PRSnapshot, now time.Time) time.Duration {
	seen, err := time.Parse(time.RFC3339, snap.ChangeSeenAt)
	if err != nil {
		return changeRefreshWindow
	}
	return now.Sub(seen)
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
