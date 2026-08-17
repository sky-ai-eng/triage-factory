package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// RemoveAt removes a worktree directory by path and prunes the bare's
// stale registration. Callers pass the actual path rather than
// deriving it from a conversationID — every previous "Remove(conversationID)" caller
// had the path in scope already, and the conversationID-only convenience was
// a footgun: it silently targeted runDir(conversationID) regardless of where
// the worktree actually lived.
//
// conversationID is used only for the log line; pass "" if not available.
func RemoveAt(path, conversationID string) error {
	if path == "" {
		return nil
	}
	// Via the privileged seam, not os.RemoveAll: on a host that sandboxes
	// (multi mode + Linux) a run tree is owned by the sandbox identity by the
	// time it's removed, with agent-created modes the post-drop orchestrator
	// cannot unlink through — so the removal executes in the cap-broker. On a
	// non-sandboxing host (local mode, or non-Linux) no sandbox ever took
	// ownership, so the seam resolves to an ordinary in-process removal — the
	// plain RemoveAll this call used to be.
	if err := sandbox.RemoveRunTree(context.Background(), path); err != nil {
		return fmt.Errorf("remove worktree dir %s: %w", path, err)
	}

	// Prune stale worktree refs from all bare repos
	if _, err := paths.StateRootErr(); err != nil {
		return err
	}
	pruneAll(paths.BareCacheRoot(runmode.LocalDefaultOrgID))

	if conversationID != "" {
		worktreeLog.Info("removed", "run_id", conversationID, "path", path)
	} else {
		worktreeLog.Info("removed", "path", path)
	}
	return nil
}

// Cleanup removes all orphaned worktrees on startup and prunes bare repos.
// Also sweeps ~/.claude/projects ghost entries for each orphaned cwd.
func Cleanup() {
	CleanupWithOptions(CleanupOptions{})
}

// CleanupOptions controls Cleanup behavior.
type CleanupOptions struct {
	// SkipClaudeProjectCleanup turns OFF deletion of every
	// ~/.claude/projects entry. Used at startup in non-local modes,
	// where the parked-worktree preserve set is keyed by the synthetic
	// sentinel org and has no real-tenant rows: rather than risk wiping
	// a session JSONL we should have kept, skip ALL project-dir cleanup
	// for this sweep. The worktree dir removal and bare-repo pruning
	// still run, so we don't leak large temp directories.
	SkipClaudeProjectCleanup bool

	// PreserveWorktreeFor names worktree directories that must survive the
	// sweep WHOLE — the directory AND its ~/.claude/projects session JSONL —
	// because the run parked `open` and the
	// worktree is the warm cache an eventual resume reuses. Unlike
	// PreserveClaudeProjectFor (which keeps only the JSONL while still deleting
	// the worktree dir), a matching entry here skips removal of both.
	//
	// Keys are worktree directory names — i.e. filepath.Base(worktree_path),
	// which is the run_id for a standalone run and the blueprint_run_id for a
	// blueprint's shared worktree. A swept (un-preserved) parked workspace
	// still resumes via snapshot rehydrate, so this is the fast path, not a
	// correctness gate. Ignored for any dir not present here.
	PreserveWorktreeFor map[string]bool
}

// CleanupWithOptions is the parameterized Cleanup the startup sweep uses
// to keep parked runs' warm worktrees alive across restarts — see
// CleanupOptions.PreserveWorktreeFor.
func CleanupWithOptions(opts CleanupOptions) {
	// Orphaned-worktree (temp-dir) cleanup. A missing runs dir just means
	// there's nothing to remove here — it must NOT skip the bare-repo
	// sweep below. /tmp is routinely wiped on reboot, which is exactly the
	// case where a locked=initializing ghost survives in the persistent
	// ~/.triagefactory/repos bare and needs reclaiming.
	runsBase := filepath.Join(os.TempDir(), runsDir)
	if entries, err := os.ReadDir(runsBase); err == nil {
		count := 0
		for _, e := range entries {
			if e.IsDir() {
				fullPath := filepath.Join(runsBase, e.Name())
				conversationID := strings.TrimSuffix(e.Name(), "-nocwd")
				// Parked runs keep their worktree WHOLE — the dir and its
				// session JSONL — as the warm resume cache, so skip them
				// entirely (no project-dir delete, no dir removal). A swept one
				// still resumes via snapshot rehydrate.
				if opts.PreserveWorktreeFor[conversationID] {
					continue
				}
				// Project-dir deletion has one opt-out: a global SkipAll
				// (non-local modes, where the preserve set can't be
				// determined). It keeps the JSONL intact while we still
				// remove the worktree dir below.
				if !opts.SkipClaudeProjectCleanup {
					// Each entry here was a live claude cwd at some point — nuke its
					// ghost ~/.claude/projects entry before removing the dir itself
					// (EvalSymlinks needs the dir to still exist to resolve).
					RemoveClaudeProjectDir(fullPath)
				}
				// Privileged seam (see RemoveAt): an orphan from a crashed
				// sandboxed run is owned by the sandbox identity, which the
				// post-drop orchestrator can't unlink itself. Errors are
				// logged rather than swallowed — a leaked tree the sweep
				// exists to reclaim should be visible, not silent.
				if err := sandbox.RemoveRunTree(context.Background(), fullPath); err != nil {
					worktreeLog.Warn("orphaned worktree not removed", "path", fullPath, "error", err)
					continue
				}
				count++
			}
		}
		if count > 0 {
			worktreeLog.Info("cleaned up orphaned worktrees", "count", count)
		}
	}

	sweepOrphanedStagingDirs(opts.PreserveWorktreeFor)

	// Prune all bare repos — always, regardless of the runs dir above.
	// Clear stale locked ghosts first (plain prune skips their lock
	// marker), then run the ordinary prune for unlocked stale entries.
	// Safe to force here: this runs at startup before any poller/spawner
	// can issue a concurrent add.
	if _, err := paths.StateRootErr(); err != nil {
		return
	}
	reposRoot := paths.BareCacheRoot(runmode.LocalDefaultOrgID)
	clearStaleLockedWorktreesAll(reposRoot)
	pruneAll(reposRoot)
}

// sweepOrphanedStagingDirs reclaims the per-run staging dirs left behind by runs
// that are gone — the step skill a launch mounts, and the prior-memory tree it
// mounts beside it. The leftover-artifact counterpart of the run-tree sweep above
// and the orphaned-cgroup sweep on the privileged side. Cheap: the dirs hold one
// SKILL.md and a handful of markdown files, but a crash loop would otherwise
// accumulate them under $TMPDIR forever.
//
// Plain os.RemoveAll, not the privileged seam: a staging dir is orchestrator-
// owned by construction and never chowned to a sandbox identity — that is the
// entire point of staging outside the run tree.
//
// preserve is the caller's warm-worktree keep set, keyed by worktree directory
// name (a run id for a standalone run, the blueprint_run_id for a blueprint's
// shared tree). Staging dirs are keyed by the step's own conversation id, so
// only the standalone shape ever matches — enough to keep the sweep from
// fighting a preserved run, and moot for the blueprint shape today because the
// only mode that stages at all (multi) passes no preserve set, so its parked
// worktrees are swept here regardless and rehydrate from snapshot.
func sweepOrphanedStagingDirs(preserve map[string]bool) {
	for _, base := range []string{sandbox.SkillStagingBase(), sandbox.MemoryStagingBase()} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue // nothing of this kind staged on this host
		}
		count := 0
		for _, e := range entries {
			if !e.IsDir() || preserve[e.Name()] {
				continue
			}
			full := filepath.Join(base, e.Name())
			if err := os.RemoveAll(full); err != nil {
				worktreeLog.Warn("orphaned staging dir not removed", "path", full, "error", err)
				continue
			}
			count++
		}
		if count > 0 {
			worktreeLog.Info("cleaned up orphaned staging dirs", "base", base, "count", count)
		}
	}
}

func pruneAll(baseDir string) {
	if err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasSuffix(path, ".git") {
			if err := gitRun(path, "worktree", "prune"); err != nil {
				worktreeLog.Warn("prune failed", "path", path, "error", err)
			}
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		worktreeLog.Warn("walk failed", "dir", baseDir, "error", err)
	}
}

// removeWorktreeRegFor deletes the single bare admin registration that
// belongs to wtDir, if present. A cancelled/killed `git worktree add`
// leaves <bare>/worktrees/<name> behind, locked with git's transient
// "initializing" marker; plain `git worktree prune` skips locked entries
// so the branch stays pinned. git names that admin dir after the
// basename of the worktree path, and our worktree paths are
// runDir(conversationID) (basename == conversationID, globally unique), so the entry is
// deterministically worktrees/<basename(wtDir)>.
//
// Targeting by path — rather than sweeping every locked=initializing
// entry in the bare — is what makes this safe to call without the
// per-repo lock: it can only ever touch this run's own dead add, never a
// concurrent add against the same bare. os.RemoveAll is a no-op when the
// entry was never created (add failed before mkdir).
func removeWorktreeRegFor(bareDir, wtDir string) {
	adminDir := filepath.Join(bareDir, "worktrees", filepath.Base(wtDir))
	if err := os.RemoveAll(adminDir); err != nil {
		worktreeLog.Warn("clear half-built worktree failed", "dir", adminDir, "error", err)
		return
	}
}

// recordedWorktreePath returns the working-tree directory a bare admin
// entry points at, read from its non-localized `gitdir` file (the path to
// <worktree>/.git that git records for every linked worktree). Returns ""
// if the gitdir file is missing or unreadable. Deliberately avoids the
// `locked` file's reason text, which git writes through gettext
// (_("initializing")) and is therefore localized.
func recordedWorktreePath(adminDir string) string {
	data, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return ""
	}
	gitPath := strings.TrimSpace(string(data))
	if gitPath == "" {
		return ""
	}
	return filepath.Dir(gitPath)
}

// isTFRunWorktreePath reports whether p is one of TF's own ephemeral run
// worktrees — i.e. a child of the triagefactory-runs temp namespace
// (runDir = <tmp>/triagefactory-runs/<conversationID>). Matched on the runsDir
// path segment rather than the full os.TempDir() prefix because $TMPDIR
// can differ between the run that created the worktree and this restart;
// the namespace itself is constant.
func isTFRunWorktreePath(p string) bool {
	return p != "" && filepath.Base(filepath.Dir(p)) == runsDir
}

// clearStaleLockedWorktrees force-removes admin registrations under
// <bareDir>/worktrees/* that are (1) locked, (2) one of TF's own
// ephemeral run worktrees, and (3) whose working tree no longer exists.
// That triple is precisely the set `git worktree prune` refuses to
// reclaim (it skips locked entries) but which is safe to drop. Returns
// the number of ghosts cleared.
//
// All three conditions matter:
//   - locked + checkout-missing is what a cancel/kill mid-`git worktree
//     add` leaves behind. We key on lock-file presence + missing checkout
//     rather than the lock reason string because git localizes that
//     reason via gettext, so the literal "initializing" isn't a stable
//     token across locales.
//   - The TF-run-namespace gate is what keeps us from violating git's
//     `worktree lock` contract: a worktree a user deliberately locked
//     because its checkout lives on a removable/network volume that's
//     merely unmounted right now also looks "locked + checkout-missing",
//     but it is NOT under triagefactory-runs, so we leave it alone.
//
// CONCURRENCY: startup-only. An in-progress `git worktree add` is briefly
// locked with its checkout not yet populated, so a concurrent sweep could
// race it — but at startup no poller/spawner has begun. The
// in-process reclaim (removeWorktreeRegFor) is the lock-free, path-scoped
// counterpart used during normal operation.
func clearStaleLockedWorktrees(bareDir string) int {
	worktreesDir := filepath.Join(bareDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		return 0 // no worktrees admin dir → nothing registered
	}
	cleared := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		adminDir := filepath.Join(worktreesDir, e.Name())
		if _, err := os.Stat(filepath.Join(adminDir, "locked")); err != nil {
			continue // unlocked → plain prune reclaims it if stale
		}
		wtPath := recordedWorktreePath(adminDir)
		if !isTFRunWorktreePath(wtPath) {
			// Locked and not one of our ephemeral run worktrees — could be
			// a user worktree on a currently-unmounted volume. Honor the
			// lock and leave it for git/the user to manage.
			continue
		}
		if _, err := os.Stat(wtPath); err == nil {
			continue // checkout still present → live worktree, never touch
		}
		if err := os.RemoveAll(adminDir); err != nil {
			worktreeLog.Warn("clear stale locked worktree failed", "dir", adminDir, "error", err)
			continue
		}
		worktreeLog.Info("cleared stale locked worktree registration", "name", e.Name())
		cleared++
	}
	return cleared
}

// clearStaleLockedWorktreesAll sweeps every bare repo under baseDir for
// stale locked worktree ghosts and force-removes them. Startup-only — see
// clearStaleLockedWorktrees for the concurrency contract. This is the
// backstop for ghosts left by ungraceful death (SIGKILL, crash, OOM,
// power loss) where no in-process cleanup ran.
func clearStaleLockedWorktreesAll(baseDir string) {
	total := 0
	if err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasSuffix(path, ".git") {
			total += clearStaleLockedWorktrees(path)
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		worktreeLog.Warn("walk failed", "dir", baseDir, "error", err)
	}
	if total > 0 {
		worktreeLog.Info("cleared stale locked worktree registrations", "count", total)
	}
}
