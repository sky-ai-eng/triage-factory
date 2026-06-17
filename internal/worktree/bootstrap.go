package worktree

import (
	"context"
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

// BootstrapBareClones is the budget-aware warmer: it pre-populates the
// bounded cache with every target's bare clone, then reclaims back down to
// the mode's policy budget so a warm pass can't blow past the per-pod cap.
// Idempotent — repeat calls are no-ops once the bare exists and origin is
// correctly configured.
//
// Once a correctness prerequisite ("bootstrap is the producer"), now an
// optional latency optimization: everything self-seeds on demand
// (delegation always did; the curator does after TFAC-60), so the warmer
// only hides the first-dispatch clone cost. It runs after repo profiling
// (so CloneURLs are populated).
//
// Iteration is serial under per-repo locks. Parallelism would only
// help the cold-start case where every repo needs its initial clone,
// and even then bandwidth is the limiting factor — serial keeps
// network pressure predictable.
//
// **Best-effort, not a hard prereq.** The warmer is purely additive:
// if a delegation arrives for a repo before the warmer reaches it,
// CreateForPR / CreateForBranch will lazily clone via the same
// per-repo lockRepo() mutex this function takes. The two paths
// serialize against each other (no double-clone) but the lazy path
// pays the cold-clone cost itself.
//
// After warming, EnforceBudget(DefaultPolicy()) reclaims the coldest
// bares if the warm pass pushed the cache over budget. In local
// (unbounded policy) that's a no-op, so local still warms + persists every
// configured repo exactly like before; in multi it keeps a large
// configured set from exceeding the per-pod disk budget.
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
			worktreeLog.Warn("warm failed", "owner", t.Owner, "repo", t.Repo, "error", err)
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
	worktreeLog.Info("warm complete", "duration", time.Since(start).Round(time.Millisecond), "ensured", ensured, "skipped", skipped, "failed", failed)

	EnforceBudget(ctx, DefaultPolicy())
}
