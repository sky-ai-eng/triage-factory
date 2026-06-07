package worktree

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// CopyForTakeover materializes a working copy of a delegated run's
// worktree at <baseDir>/run-<runID>/ so the user can resume the headless
// Claude Code session interactively. Returns the absolute destination
// path on success.
//
// The destination is a *linked worktree* of the same bare repo the
// original run used — the same lightweight pattern CreateForPR /
// CreateForBranch already use. `.git` in the destination is a 1-line
// pointer file referencing <bareDir>/worktrees/run-<runID>/, NOT a
// full git directory. Trade-offs vs. a full clone:
//
//   - Near-instant. No object copying, no blob materialization, no
//     network. For a multi-GB repo the difference is "minutes" vs.
//     "milliseconds." This is the whole point of the change.
//   - Full git functionality: status, diff, log, commit, push to the
//     bare's origin (i.e. GitHub) all work normally, because the bare
//     repo is the worktree's gitdir.
//   - Coupled to TF's repo cache: if the user wipes
//     ~/.triagefactory/repos, the takeover dir breaks. The modal
//     surfaces this so the user can `git clone` to detach if they want
//     a fully standalone copy.
//
// We add the linked worktree on the SAME branch the agent was using so
// the user can continue and push back without extra steps. This is
// safe because Spawner.Takeover removes the original /tmp worktree
// before returning — git allows multiple worktrees on the same branch
// only one at a time, but the original is gone by the time the user
// touches the takeover dir.
//
// `--no-checkout` skips materializing files from HEAD (which would
// trigger lazy blob fetching from the partial-clone bare). We then
// overlay the agent's working-tree state from the source worktree —
// modified, added, and untracked files, minus the managed scratch
// directories — so the user sees exactly what the agent was looking
// at when takeover happened.
//
// Files matching managedExcludePatterns (_scratch/) are not copied.
// The destination's .git/info/exclude inherits the bare's
// configuration; we re-write our managed block so those paths stay
// hidden from `git status` in the takeover dir as well.
func CopyForTakeover(ctx context.Context, runID, srcWorktree, baseDir string) (string, error) {
	if runID == "" {
		return "", fmt.Errorf("takeover: empty run id")
	}
	if srcWorktree == "" {
		return "", fmt.Errorf("takeover: empty source worktree path")
	}
	// Reject empty baseDir explicitly. filepath.Abs("") silently
	// returns the binary's current working directory, so a caller
	// that bypassed Spawner.Takeover's validation would create
	// takeovers wherever the user launched the binary from — bad
	// surprise for anything from /usr/local/bin to ~/Downloads.
	// Spawner.Takeover catches this before us, but the helper
	// shouldn't trust the upstream.
	if baseDir == "" {
		return "", fmt.Errorf("takeover: empty destination base dir")
	}

	// Resolve to an absolute path before doing anything else. The
	// returned path ends up in the resume command shown to the user;
	// if takeover_dir is configured as a relative path, a relative
	// destination would only work if the user pasted the command from
	// the same cwd the binary was launched from. Make it cwd-independent.
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("takeover: resolve base dir: %w", err)
	}
	destDir := filepath.Join(absBase, "run-"+runID)
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return "", fmt.Errorf("takeover: mkdir base: %w", err)
	}
	// Check destination existence BEFORE touching the source. Re-takeover
	// for the same runID must be idempotent: the move path renames the
	// source out of /tmp on the first call, so a naive second call would
	// fail validating the now-missing source even though the work is
	// already done.
	if _, err := os.Stat(destDir); err == nil {
		// A previous takeover for the same run was already materialized
		// — IF the existing path is actually a usable worktree. Validate
		// it before returning. A regular file at this path (typo, errant
		// `touch`), an empty directory left behind by an interrupted
		// previous attempt, or a directory whose `.git` pointer
		// references a gitdir that no longer exists would all "succeed"
		// the bare stat check but be useless for resume — the user
		// would `cd` in and find nothing works.
		if err := validateExistingTakeoverDest(ctx, destDir); err != nil {
			return "", fmt.Errorf("takeover: existing destination at %s is not a usable worktree (%w); remove or rename it and retry", destDir, err)
		}
		// Returning the existing path is the right call here — re-
		// invoking the endpoint shouldn't clobber a working copy the
		// user may already have local edits in.
		return destDir, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("takeover: stat destination: %w", err)
	}

	srcInfo, err := os.Stat(srcWorktree)
	if err != nil {
		return "", fmt.Errorf("takeover: stat source worktree: %w", err)
	}
	if !srcInfo.IsDir() {
		return "", fmt.Errorf("takeover: source worktree is not a directory: %s", srcWorktree)
	}

	bareDir, err := resolveBareDirFromWorktree(srcWorktree)
	if err != nil {
		return "", fmt.Errorf("takeover: locate bare repo: %w", err)
	}

	branch, err := currentBranch(ctx, srcWorktree)
	if err != nil {
		return "", fmt.Errorf("takeover: read current branch: %w", err)
	}

	// Fast path: when source and destination are on the same filesystem,
	// `git worktree move` is an atomic rename(2) and lets us avoid the
	// overlay copy entirely. The branch registration moves with the
	// worktree so there's no co-checkout situation to bypass with --force.
	moved, err := tryMoveWorktree(ctx, bareDir, srcWorktree, destDir)
	if err != nil {
		return "", fmt.Errorf("takeover: move worktree: %w", err)
	}
	if moved {
		// _scratch/ traveled with the move; the overlay path skips it
		// explicitly because it's TF infra and shouldn't land in the
		// user's hands. Mirror that here so behavior is the same
		// regardless of which path we took.
		_ = os.RemoveAll(filepath.Join(destDir, "_scratch"))
		// Refresh the managed exclude block in case managedExcludePatterns
		// has grown since the agent originally wrote it. Best-effort.
		if err := writeLocalExcludes(destDir); err != nil {
			log.Printf("[worktree] warning: takeover %s: refresh excludes after move: %v", runID, err)
		}
		log.Printf("[worktree] takeover via move: %s -> %s (branch %s)", srcWorktree, destDir, branch)
		return destDir, nil
	}

	// Fallback: cross-filesystem move isn't supported by git (rename(2)
	// returns EXDEV). Add a fresh linked worktree at the destination and
	// overlay the agent's working tree. Slower than the rename path but
	// correctness-equivalent for users whose $TMPDIR and takeover_dir
	// live on different filesystems.
	if err := addAndOverlayForTakeover(ctx, runID, bareDir, srcWorktree, destDir, branch); err != nil {
		return "", err
	}

	// The overlay path leaves the original worktree in place because we
	// needed its files for the copy. Now that the copy is done, remove
	// the source worktree dir explicitly using its actual path —
	// callers can pass an arbitrary srcWorktree, so deriving the path
	// from runID via Remove() would risk removing the wrong directory
	// if the source ever differs from runDir(runID). (The move path
	// doesn't need this — git already renamed the source out from
	// under us.)
	if err := RemoveAt(srcWorktree, runID); err != nil {
		log.Printf("[worktree] warning: takeover %s: remove original after overlay: %v", runID, err)
	}

	log.Printf("[worktree] takeover via overlay: %s -> %s (branch %s)", srcWorktree, destDir, branch)
	return destDir, nil
}

// validateExistingTakeoverDest checks that a path which already exists
// at the takeover destination is healthy enough to resume from.
// "Healthy" means: it's a directory, and `git rev-parse --git-dir`
// succeeds inside it. The git probe is cheap (sub-millisecond) and
// catches the cases a bare os.Stat doesn't:
//
//   - Path is a regular file (returned because os.Stat doesn't care).
//     User would `cd` into nothing.
//   - Path is an empty directory left over from an interrupted previous
//     takeover (stat passes but there's no git metadata).
//   - Path has a `.git` pointer file referencing a gitdir that no
//     longer exists (user wiped ~/.triagefactory/repos, or the bare
//     was deleted). Pointer parses fine, but git ops fail.
//
// On any of these we return an error so CopyForTakeover surfaces a
// clear message asking the user to remove or rename the existing
// path. We deliberately don't auto-recover by deleting — the user may
// have intentionally placed something there (e.g., manually inspected
// a previous takeover), and silent destruction would be hostile.
func validateExistingTakeoverDest(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if _, err := gitOutputCtx(ctx, path, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("not a git worktree: %w", err)
	}
	return nil
}

// tryMoveWorktree attempts `git worktree move <src> <dest>` and returns
// (true, nil) on success, (false, nil) when the move is unsupported
// because src and dest are on different filesystems (git uses rename(2)
// which returns EXDEV cross-fs and refuses to fall back), and
// (false, err) for any other failure.
//
// The same-filesystem check is done up front via syscall.Stat_t.Dev so
// we don't have to parse git's stderr for "Invalid cross-device link"
// strings that vary across platforms and locales. The check looks at
// the parents of src and dest because dest doesn't exist yet — its
// device id is determined by the filesystem its parent lives on.
func tryMoveWorktree(ctx context.Context, bareDir, src, dest string) (bool, error) {
	sameFS, err := sameFilesystem(src, filepath.Dir(dest))
	if err != nil {
		// Stat of either path failed (src missing, dest parent missing).
		// Surface as a real error rather than silently falling through —
		// the overlay path would hit the same problem.
		return false, fmt.Errorf("same-fs check: %w", err)
	}
	if !sameFS {
		return false, nil
	}
	if err := gitRunCtx(ctx, bareDir, "worktree", "move", src, dest); err != nil {
		return false, fmt.Errorf("git worktree move: %w", err)
	}
	return true, nil
}

// sameFilesystem returns true iff a and b live on the same filesystem,
// determined by syscall.Stat_t.Dev. Used by the takeover-move pre-flight
// check; rename(2) refuses cross-device moves and we'd rather know up
// front than parse a platform-specific error message.
func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}

// addAndOverlayForTakeover is the cross-filesystem fallback for
// CopyForTakeover. Adds a fresh linked worktree at destDir (with --force
// because the source is still registered on the same branch) and
// overlays the agent's working tree on top. Caller is responsible for
// removing the source worktree afterward.
func addAndOverlayForTakeover(ctx context.Context, runID, bareDir, srcWorktree, destDir, branch string) error {
	// Linked-worktree add against the same bare. --no-checkout means git
	// only writes the gitdir + .git pointer; the working tree starts
	// empty and we fill it via overlayWorkingTree. Without --no-checkout
	// git would lazy-fetch blobs from the partial-clone bare, which is
	// the slow path we're explicitly avoiding.
	//
	// --force bypasses git's "branch is already checked out at <path>"
	// safety check. The original /tmp worktree IS still registered with
	// the bare repo at this point: Spawner.Takeover SIGKILLs the agent
	// before calling us but defers the worktree.Remove until after this
	// returns (we need the source files for overlayWorkingTree). Without
	// --force, git would refuse the add because the branch is co-checked
	// out by the original. The safety the flag bypasses — "don't let two
	// live worktrees fight over a branch" — doesn't apply here: the
	// agent is dead, no process is writing to the original, and the
	// dual-worktree state lasts only as long as the overlay copy.
	args := []string{"worktree", "add", "--force", "--no-checkout"}
	if branch != "" {
		args = append(args, destDir, branch)
	} else {
		// Detached HEAD on the source — give the destination a detached
		// HEAD too. We pass the source's HEAD commit explicitly because
		// `git worktree add --detach` without a commit-ish falls back
		// to the bare's HEAD, which may not resolve to a valid commit
		// for our partial-clone bare. Resolving from the source is
		// always correct: that's the commit the agent was sitting on.
		commitOut, err := gitOutputCtx(ctx, srcWorktree, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("takeover: resolve source HEAD: %w", err)
		}
		commit := strings.TrimSpace(commitOut)
		if commit == "" {
			return fmt.Errorf("takeover: source HEAD resolved to empty commit")
		}
		args = append(args, "--detach", destDir, commit)
	}
	if err := gitRunCtx(ctx, bareDir, args...); err != nil {
		return fmt.Errorf("takeover: worktree add: %w", err)
	}

	// Re-apply the managed exclude block (_scratch/) so `git status` in
	// the takeover dir doesn't surface our infra dirs even if the user
	// ends up needing them. Best-effort: a failure here is annoying but
	// not fatal — git status will just be a bit noisy.
	if err := writeLocalExcludes(destDir); err != nil {
		log.Printf("[worktree] warning: takeover %s: write excludes: %v", runID, err)
	}

	if err := overlayWorkingTree(srcWorktree, destDir); err != nil {
		return fmt.Errorf("takeover: overlay working tree: %w", err)
	}

	return nil
}

// resolveBareDirFromWorktree returns the bare repo behind a linked
// worktree by asking git directly via `rev-parse --git-common-dir` —
// git's canonical answer to "where is the shared object/refs store"
// for both linked worktrees and plain checkouts. Avoids parsing the
// `.git` pointer file ourselves, which would couple us to git's on-
// disk layout (`.git/worktrees/<name>/`) that may shift between git
// versions.
func resolveBareDirFromWorktree(wtDir string) (string, error) {
	commonDir, err := gitOutput(wtDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("rev-parse --git-common-dir: %w", err)
	}
	commonDir = strings.TrimSpace(commonDir)
	if commonDir == "" {
		return "", fmt.Errorf("git-common-dir returned empty")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(wtDir, commonDir)
	}
	abs, err := filepath.Abs(commonDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("git common dir %s does not exist: %w", abs, err)
	}
	return abs, nil
}

func currentBranch(ctx context.Context, wtDir string) (string, error) {
	out, err := gitOutputCtx(ctx, wtDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "HEAD" {
		// Detached HEAD — return empty so the caller can detect the
		// detached state and handle it explicitly (for example by resolving
		// the source HEAD commit), rather than relying on a clone default.
		return "", nil
	}
	return branch, nil
}

// overlayWorkingTree copies every file from src onto dest, skipping the
// `.git` directory (the destination already has its own git metadata
// from the linked-worktree add) and skipping managedExcludePatterns
// (those are TF infrastructure dirs, not user-relevant state).
//
// Because the destination was created with `git worktree add
// --no-checkout`, its working tree starts empty. After this overlay
// runs, the working tree contains exactly what the agent's worktree
// contained — no more, no less. From git's POV: files modified in src
// show as modified in dest, untracked files in src show as untracked
// in dest, and files the agent deleted (present at HEAD but absent
// from src) show as deleted in dest. Deletions are represented
// implicitly because we never put them in dest in the first place.
func overlayWorkingTree(src, dest string) error {
	skipNames := map[string]bool{".git": true}
	for _, pat := range managedExcludePatterns {
		// managedExcludePatterns end with '/' — strip for filename match
		skipNames[strings.TrimSuffix(pat, "/")] = true
	}

	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}

	return filepath.Walk(srcAbs, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// A file vanishing between readdir and lstat (e.g. the agent
			// or a child subprocess flushing writes during the brief
			// window before SIGKILL is reaped) is recoverable — we
			// just skip the entry and continue. Anything else is fatal.
			if os.IsNotExist(walkErr) {
				log.Printf("[worktree] takeover overlay: skipping vanished entry %s", path)
				return nil
			}
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Top-level skips: .git, _scratch
		topSegment := rel
		if i := strings.Index(rel, string(filepath.Separator)); i >= 0 {
			topSegment = rel[:i]
		}
		if skipNames[topSegment] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destPath := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(destPath, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			_ = os.Remove(destPath)
			return os.Symlink(target, destPath)
		}
		if err := copyFile(path, destPath, info.Mode().Perm()); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return nil
	})
}

// copyFile copies a regular file from src to dst, preserving the mode.
// Deliberately does NOT call out.Sync(): per-file fsync would serialize
// with disk on every iteration of the overlay walk (thousands of small
// files for a typical repo) for no durability benefit in this context.
// The takeover destination is a "user-about-to-use-this" workspace, not
// a crash-recovery target — a kernel crash mid-overlay would just have
// the user re-run takeover. Normal page-cache write-back is sufficient,
// and matches the move path which doesn't fsync either (rename(2) is
// atomic for the metadata, but the data writes the original agent did
// were never per-file fsync'd either).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// WorktreeBranch returns the name of the branch checked out at the given
// worktree path. Empty result + non-nil error means the path isn't a
// worktree (or git refused). The caller uses this to learn the local
// branch name without consulting the GitHub API — important for the
// release path, which has to reclaim the bare's branch ref before the
// fetch-into-checked-out-branch error can clear, and a network call
// would make release fail when GitHub is unreachable.
func WorktreeBranch(path string) (string, error) {
	out, err := gitOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "HEAD" {
		// Detached HEAD — no branch name to clean up.
		return "", nil
	}
	return branch, nil
}
