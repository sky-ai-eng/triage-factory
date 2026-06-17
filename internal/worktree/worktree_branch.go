package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CreateForBranch sets up a worktree on a new feature branch based off
// a given base, at the run's default location runDir(runID). If
// baseBranch is empty, the repo's default branch is detected from
// origin/HEAD. Used by the eager GitHub PR delegation path where the
// run has exactly one repo.
func CreateForBranch(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID string, opts ...CloneOption) (string, error) {
	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}
	return createBranchWorktreeAt(ctx, owner, repo, cloneURL, baseBranch, featureBranch, runID, wtDir, resolveCloneOptions(opts).auth)
}

// CreateForBranchInRoot is the lazy-Jira-delegation variant: the worktree
// lands at filepath.Join(runRoot, owner, repo) so a single run can host
// multiple per-repo worktrees as siblings under a shared run-root. The
// run-root must already exist (created by MakeRunRoot in the spawner);
// the owner-level subdir is created here.
//
// Other than the path, behavior matches CreateForBranch — same per-repo
// lock, same bare-clone reuse, same branch-exists reattach, same excludes
// rollback. The Jira `workspace add` CLI is the sole caller in production.
func CreateForBranchInRoot(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, runRoot string) (string, error) {
	if runRoot == "" {
		return "", fmt.Errorf("CreateForBranchInRoot: runRoot is required")
	}
	wtDir := filepath.Join(runRoot, owner, repo)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir owner subdir: %w", err)
	}
	// No CloneAuth here: this is the in-sandbox Jira `workspace add` path
	// (cmd/exec/workspace), where in-sandbox git credentials are SKY-394's
	// concern, not the host-side clone path. The shared body is already
	// auth-capable, so SKY-394 can thread a credential through when it wires
	// the in-sandbox path.
	return createBranchWorktreeAt(ctx, owner, repo, cloneURL, baseBranch, featureBranch, runID, wtDir, CloneAuth{})
}

// createBranchWorktreeAt is the shared body of the two CreateForBranch
// variants — bare-clone setup, base-branch fetch, `git worktree add`
// (with branchExists reattach), and exclude-or-rollback. The two
// public callers differ only in where wtDir lives on disk.
func createBranchWorktreeAt(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, wtDir string, auth CloneAuth) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, cloneURL, auth)
	if err != nil {
		return "", err
	}

	// Fetch the base branch into the remote-tracking ref rather than
	// the local branch ref. The Curator's per-project worktrees
	// (EnsureCuratorWorktree) check out the base branch as a real local
	// branch in <projectDir>/repos/<owner>/<repo>/; if we fetched with
	// `+refs/heads/<b>:refs/heads/<b>`, git would refuse with "fatal:
	// refusing to fetch into branch '<b>' checked out at '<path>'"
	// because that local branch ref is live in the curator's worktree.
	// Fetching into refs/remotes/origin/<b> sidesteps the conflict and
	// matches the pattern EnsureCuratorWorktree already uses (see
	// internal/worktree/curator.go:93-100). The new feature branch is
	// then created off the just-fetched remote-tracking ref.
	if baseBranch == "" {
		baseBranch = detectDefaultBranch(ctx, bareDir)
	}
	remoteRef := "refs/remotes/origin/" + baseBranch
	baseRef := fmt.Sprintf("+refs/heads/%s:%s", baseBranch, remoteRef)
	start := time.Now()
	if err := gitRunCtxAuth(ctx, bareDir, auth, "fetch", "origin", baseRef); err != nil {
		return "", fmt.Errorf("fetch base branch %s: %w", baseBranch, err)
	}
	worktreeLog.Debug("fetch completed", "branch", baseBranch, "duration", time.Since(start).Round(time.Millisecond))

	// Create worktree — reuse the branch if it already exists (re-delegation),
	// otherwise create a new one off the just-fetched remote-tracking ref.
	// Both routed through gitRunCtxAuth so the blobless bare's lazy promisor
	// fetch (working-tree blobs the partial clone deferred) authenticates
	// against origin on a private repo; auth is inert for local/public clones.
	if branchExists(bareDir, featureBranch) {
		// Branch exists from a previous run — check it out
		if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", wtDir, featureBranch); err != nil {
			return "", fmt.Errorf("worktree add existing branch: %w", err)
		}
	} else {
		if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", "-b", featureBranch, wtDir, remoteRef); err != nil {
			return "", fmt.Errorf("worktree add new branch: %w", err)
		}
	}

	if err := addExcludesOrRollback(runID, wtDir); err != nil {
		return "", err
	}

	worktreeLog.Debug("branch worktree at", "dir", wtDir, "branch", featureBranch, "base", baseBranch)
	return wtDir, nil
}

// addExcludesOrRollback wraps writeLocalExcludes with the rollback both
// Create* functions need: if the exclude write fails, the worktree is
// already registered with the bare repo and on disk, so we must remove
// it before returning. Without rollback the caller sees an error but
// has no handle to clean up with, leaking a half-configured worktree
// and its bare-repo registration.
func addExcludesOrRollback(runID, wtDir string) error {
	if err := writeLocalExcludes(wtDir); err != nil {
		if rmErr := RemoveAt(wtDir, runID); rmErr != nil {
			worktreeLog.Warn("rollback after exclude-write failure", "error", rmErr)
		}
		return fmt.Errorf("write local git excludes: %w", err)
	}
	return nil
}

// managedExcludePatterns are the gitignore patterns writeLocalExcludes
// ensures are present in .git/info/exclude for every delegated worktree.
//
//   - _scratch/ — CI log archives, ephemeral downloads (SKY-146), entity-memory
//     and project-knowledge subdirs populated by the spawner (SKY-219).
//     One prefix covers everything under it.
var managedExcludePatterns = []string{"_scratch/"}

// Markers delimiting the managed section of .git/info/exclude. writeLocalExcludes
// rewrites the content between these markers in place when both are present,
// and appends a fresh marker block otherwise. Using explicit markers means
// the managed section remains a self-contained complete manifest of our
// patterns regardless of how managedExcludePatterns evolves — growing the
// list reuses the existing section instead of appending a second header.
const (
	managedExcludeBegin = "# triagefactory: begin managed exclude block (do not edit)"
	managedExcludeEnd   = "# triagefactory: end managed exclude block"
)

// writeLocalExcludes ensures the worktree's .git/info/exclude file contains
// every pattern in managedExcludePatterns so agents can't accidentally
// commit our infrastructure directories.
//
// Content outside our marked section is never touched: user patterns,
// tool-managed lines from other tools, and git's stock comment header
// are all preserved verbatim. Only the lines between managedExcludeBegin
// and managedExcludeEnd get rewritten, and only if the rewritten content
// differs from what's already there. On a file that doesn't yet have the
// markers, the managed section is appended at EOF in a single pass. On
// subsequent runs the markers exist, so we replace in place — which means
// growing managedExcludePatterns expands the section rather than tacking
// a duplicate header at the end of the file.
//
// Uses .git/info/exclude rather than a committed .gitignore because these
// paths are infrastructure concerns, not something the tracked repo should
// know or care about.
//
// Fails closed: if any step fails we return the error and the caller is
// responsible for rolling back the partially-created worktree. A worktree
// without the excludes is a footgun (agents could commit hundreds of log
// files), so rolling back the worktree on error is the safer behavior
// than silently proceeding.
//
// Worktrees in git use a per-worktree info directory — for a linked
// worktree, `.git` is a file containing `gitdir: <path>`, and
// `info/exclude` lives under that gitdir. For a plain checkout `.git` is
// a directory. Both layouts are handled.
func writeLocalExcludes(wtDir string) error {
	excludePath, err := resolveExcludePath(wtDir)
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read exclude file: %w", err)
	}
	existingStr := string(existing)

	// Build the canonical managed block from the current pattern list.
	// Always written as a complete manifest — never a delta — so a
	// growing managedExcludePatterns just expands this same block rather
	// than accumulating multiple header sections over time.
	var block strings.Builder
	block.WriteString(managedExcludeBegin)
	block.WriteString("\n")
	for _, p := range managedExcludePatterns {
		block.WriteString(p)
		block.WriteString("\n")
	}
	block.WriteString(managedExcludeEnd)
	block.WriteString("\n")
	managedBlock := block.String()

	newContent, changed := mergeManagedBlock(existingStr, managedBlock)
	if !changed {
		return nil // file already contains exactly this managed block; no-op
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0755); err != nil {
		return fmt.Errorf("mkdir info dir: %w", err)
	}
	if err := os.WriteFile(excludePath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("write exclude file: %w", err)
	}
	return nil
}

// mergeManagedBlock returns the updated file contents with managedBlock
// installed, and a bool indicating whether the content actually changed
// (used for idempotency — we skip the rewrite if the file is already
// what we want).
//
// Marker search is direction-aware in two ways:
//
//  1. We find the begin marker via LastIndex, not Index. If the file has
//     an earlier stray or orphaned begin marker (a truncated block whose
//     end was hand-deleted, a quoted reference in a user comment, stale
//     content from a broken previous run), matching the *first* begin
//     would pair it with the real end marker later in the file and
//     clobber every line in between — violating the "content outside our
//     marked section is never touched" guarantee. LastIndex locks onto
//     the most recent begin, leaving any stray earlier markers and the
//     user content around them untouched.
//
//  2. We find the end marker via Index on the slice *after* the begin
//     position. Searching the whole file for end would pick up the first
//     occurrence, which could sit before begin in unrelated content. The
//     earlier-end + later-begin pair would look malformed, causing us to
//     append a duplicate managed block every run.
//
// If a valid begin...end pair is found, the bytes between them (plus the
// trailing newline after end) are replaced with managedBlock. Everything
// outside the markers is preserved byte-for-byte. If no valid pair
// exists, managedBlock is appended at EOF with a blank-line separator.
//
// Known limitation: a file with a genuinely duplicate valid managed
// block (two complete begin...end pairs) has only its last pair rewritten
// on each run. Earlier blocks remain as orphaned duplicates, which git
// dedupes internally for gitignore purposes but looks ugly to a human
// reader. We don't expect to produce this state ourselves — only hand
// editing could cause it, and the cleanup is a manual edit.
func mergeManagedBlock(existing, managedBlock string) (string, bool) {
	beginIdx := strings.LastIndex(existing, managedExcludeBegin)
	if beginIdx >= 0 {
		searchFrom := beginIdx + len(managedExcludeBegin)
		if relEnd := strings.Index(existing[searchFrom:], managedExcludeEnd); relEnd >= 0 {
			endIdx := searchFrom + relEnd
			// Consume up to and including the newline that follows the
			// end marker so the final structure is
			// [before][managedBlock][after] without introducing or losing
			// blank lines at the seams.
			afterEnd := endIdx + len(managedExcludeEnd)
			if afterEnd < len(existing) && existing[afterEnd] == '\n' {
				afterEnd++
			}
			candidate := existing[:beginIdx] + managedBlock + existing[afterEnd:]
			if candidate == existing {
				return existing, false
			}
			return candidate, true
		}
	}

	// No valid marker pair found. Append the managed block at EOF,
	// ensuring the pre-existing content is newline-terminated and
	// separated from our block by a blank line for readability.
	var suffix strings.Builder
	if existing != "" {
		if !strings.HasSuffix(existing, "\n") {
			suffix.WriteString("\n")
		}
		suffix.WriteString("\n")
	}
	suffix.WriteString(managedBlock)
	return existing + suffix.String(), true
}

// resolveExcludePath returns the filesystem path of .git/info/exclude for
// a worktree, handling both the linked-worktree case (where .git is a
// pointer file) and the plain-checkout case (where .git is a directory).
//
// The linked-worktree branch parses only the first line of the pointer
// file (git's canonical format is exactly `gitdir: <path>\n`, but some
// third-party tools append extra config to the same file — we ignore
// anything past the first newline). It then validates:
//
//  1. The first line starts with "gitdir:". Without this check a
//     corrupted or non-pointer file would have its content interpreted
//     as a literal path and we'd write to an arbitrary disk location.
//  2. The parsed gitdir already exists as a directory. An otherwise-
//     valid-looking pointer referencing a missing or file-shaped
//     target would silently get its parent created by MkdirAll on the
//     write path — rejecting here prevents that.
func resolveExcludePath(wtDir string) (string, error) {
	gitFile := filepath.Join(wtDir, ".git")
	info, err := os.Stat(gitFile)
	if err != nil {
		return "", fmt.Errorf("stat .git: %w", err)
	}
	if info.IsDir() {
		// Plain checkout
		return filepath.Join(gitFile, "info", "exclude"), nil
	}
	// Linked worktree: .git is a pointer file like "gitdir: /path/to/worktrees/<name>"
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return "", fmt.Errorf("read .git pointer: %w", err)
	}
	// Only the first line is part of the gitdir pointer. Anything past
	// the first newline is unrelated content (extra config some tools
	// write) and we ignore it.
	firstLine := string(data)
	if nl := strings.IndexByte(firstLine, '\n'); nl >= 0 {
		firstLine = firstLine[:nl]
	}
	firstLine = strings.TrimSpace(firstLine)
	const prefix = "gitdir:"
	if !strings.HasPrefix(firstLine, prefix) {
		return "", fmt.Errorf(".git file is not a valid worktree pointer (missing %q prefix): %q", prefix, firstLine)
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(firstLine, prefix))
	if gitdir == "" {
		return "", fmt.Errorf(".git pointer has empty gitdir path")
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(wtDir, gitdir)
	}
	// Validate the referenced gitdir actually exists as a directory
	// before we return a path inside it. Without this, a pointer file
	// with a bogus (but prefix-valid) target would pass the textual
	// checks above and silently get its info/ parent created via
	// MkdirAll on the write path — writing to an arbitrary location
	// under that target.
	gitdirInfo, err := os.Stat(gitdir)
	if err != nil {
		return "", fmt.Errorf(".git pointer references missing gitdir %q: %w", gitdir, err)
	}
	if !gitdirInfo.IsDir() {
		return "", fmt.Errorf(".git pointer references %q which is not a directory", gitdir)
	}
	return filepath.Join(gitdir, "info", "exclude"), nil
}

// branchExists checks whether a branch ref exists in the bare repo.
func branchExists(bareDir, branch string) bool {
	err := gitRun(bareDir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// detectDefaultBranch reads HEAD from the bare repo to find the default branch.
// In a bare clone, HEAD points directly to refs/heads/<default> (not refs/remotes/origin/*).
// Falls back to "main" if detection fails.
func detectDefaultBranch(ctx context.Context, bareDir string) string {
	// Routed through gitOutputCtx (like every other git call in this package)
	// so error handling stays consistent; detection failure falls back to "main".
	out, err := gitOutputCtx(ctx, bareDir, "symbolic-ref", "HEAD")
	if err == nil {
		// Output is like "refs/heads/main\n"
		ref := strings.TrimSpace(out)
		if strings.HasPrefix(ref, "refs/heads/") {
			return ref[len("refs/heads/"):]
		}
	}
	return "main"
}
