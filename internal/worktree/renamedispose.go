package worktree

import (
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// DisposeRenamedRepoDirs removes the directories a repository rename orphaned
// under its OLD slug: the bare clone (BareCacheDir) and, with it, every cold
// checkout the bare still registers — which includes the shared curator
// worktree, a linked worktree registered against the bare like any other.
// Reports whether anything was actually removed.
//
// This is the rename counterpart of evictBare, holding its exact invariant: the
// per-repo lock is taken and the live-worktree guard re-checked under it, so a
// tree a running agent or an active curator session is reading is never
// touched. A blocked disposal returns false and nothing retries it — after the
// rename applies, every subsequent observation is the steady-state no-op — so
// that residual leak is accepted in exchange for never deleting under a live
// run. Local mode is where this call matters: the reaper's policy there is
// deliberately unbounded, so nothing else would ever reclaim the orphan. In
// multi the caller reaches at most its own pod's disk and the TTL reaper
// remains the fleet-wide answer; a directory that isn't on this disk is simply
// a no-op here.
//
// The old slug names nothing after the rename commits — the registry row
// resolves under the new slug, and new clones derive new paths — which is what
// makes deleting by name safe: the only trees these paths can address are the
// orphaned ones.
func DisposeRenamedRepoDirs(orgID, owner, repo string) bool {
	if _, err := paths.StateRootErr(); err != nil {
		return false
	}
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	removed := false
	bare := paths.BareCacheDir(orgID, owner, repo)
	switch _, err := os.Stat(bare); {
	case err == nil:
		if bareHasLiveWorktrees(bare) {
			worktreeLog.Warn("renamed repo's old tree left in place; a live worktree holds it",
				"owner", owner, "repo", repo, "bare", bare)
			return false
		}
		// The guard proved every registered checkout is cold, so remove them
		// with the bare — the shared curator checkout among them — exactly as
		// eviction would.
		removeRegisteredWorktrees(bare)
		if err := os.RemoveAll(bare); err != nil {
			worktreeLog.Warn("remove renamed repo's bare failed", "bare", bare, "error", err)
		} else {
			forgetBare(bare)
			removed = true
		}
	case !os.IsNotExist(err):
		// Can't tell whether a tree is even there. Fail toward keeping it —
		// the reaper's direction too — but visibly: a permission or I/O fault
		// here means an orphan this call exists to reclaim may be leaking.
		worktreeLog.Warn("stat renamed repo's bare failed", "bare", bare, "error", err)
	}

	// A shared curator checkout that outlived its bare (the registration is
	// what normally ties the two lifetimes together) is unreachable through
	// any registry and reclaimed here directly — but only when quiescent: the
	// per-repo lock held above serializes with EnsureSharedCuratorWorktree,
	// which takes the same lock before it can bump the reader count.
	shared := paths.CuratorSharedRepoDir(orgID, owner, repo)
	switch _, err := os.Stat(shared); {
	case err == nil:
		if sharedReadersCount(shared) > 0 {
			worktreeLog.Warn("renamed repo's shared curator checkout left in place; readers hold it",
				"owner", owner, "repo", repo, "dir", shared)
			return removed
		}
		if err := os.RemoveAll(shared); err != nil {
			worktreeLog.Warn("remove renamed repo's curator checkout failed", "dir", shared, "error", err)
		} else {
			removed = true
		}
	case !os.IsNotExist(err):
		worktreeLog.Warn("stat renamed repo's curator checkout failed", "dir", shared, "error", err)
	}
	return removed
}
