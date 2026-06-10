package worktree

import (
	"context"
	"log"
	"time"
)

// BootstrapTarget is a single repo for BootstrapBareClones to ensure
// on disk. Owner and Repo identify the bare's path; CloneURL is the
// upstream URL that origin should point at. Empty CloneURL means
// profiling hasn't populated the URL yet — those targets are skipped
// rather than guessed at, and the next delegated run on that repo
// will lazily clone once profiling has caught up.
type BootstrapTarget struct {
	Owner    string
	Repo     string
	CloneURL string
}

// BootstrapBareClones ensures every target has a bare clone with the
// configured upstream URL. Idempotent — repeat calls are no-ops once
// the bare exists and origin is correctly configured.
// Intended to run after repo profiling completes (so CloneURLs are
// populated), removing the first-delegation clone latency that the
// lazy path inside CreateForPR / CreateForBranch would otherwise pay.
//
// Iteration is serial under per-repo locks. Parallelism would only
// help the cold-start case where every repo needs its initial clone,
// and even then bandwidth is the limiting factor — serial keeps
// network pressure predictable.
//
// **Best-effort, not a hard prereq.** Bootstrap is purely additive:
// if a delegation arrives for a repo before bootstrap reaches it,
// CreateForPR / CreateForBranch will lazily clone via the same
// per-repo lockRepo() mutex this function takes. The two paths
// serialize against each other (no double-clone) but the lazy path
// pays the cold-clone cost itself. That's the same behavior as
// before bootstrap existed; gating delegations on bootstrap completion
// would only convert a "slow first delegation" into a hang if
// profiling/cloning ever fails, which is strictly worse.
func BootstrapBareClones(ctx context.Context, targets []BootstrapTarget) {
	if len(targets) == 0 {
		return
	}
	start := time.Now()
	ensured, skipped, failed := 0, 0, 0
	for _, t := range targets {
		if t.CloneURL == "" {
			skipped++
			continue
		}
		if _, err := EnsureBareClone(ctx, t.Owner, t.Repo, t.CloneURL); err != nil {
			log.Printf("[worktree] bootstrap %s/%s: %v", t.Owner, t.Repo, err)
			failed++
			continue
		}
		// Reclaim per-PR config blocks left over from runs that
		// didn't reach inline cleanup — runs cancelled above the
		// runAgent defer, crashed runs, etc. Safe to call whether or
		// not anything's there to clean.
		SweepStaleForkPRConfig(t.Owner, t.Repo)
		ensured++
	}
	log.Printf("[worktree] bootstrap complete in %s (%d ensured, %d skipped no-url, %d failed)",
		time.Since(start).Round(time.Millisecond), ensured, skipped, failed)
}
