package worktree

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// claudeProjectsDir is where Claude Code auto-creates per-cwd session history.
const claudeProjectsDir = ".claude/projects"

// Per-repo mutexes prevent concurrent fetches from racing on the same bare repo.
var (
	repoMu    sync.Mutex
	repoLocks = map[string]*sync.Mutex{}
)

func lockRepo(owner, repo string) *sync.Mutex {
	key := owner + "/" + repo
	repoMu.Lock()
	defer repoMu.Unlock()
	mu, ok := repoLocks[key]
	if !ok {
		mu = &sync.Mutex{}
		repoLocks[key] = mu
	}
	return mu
}

const (
	reposDir = ".todotriage/repos" // bare clones: ~/.todotriage/repos/{owner}/{repo}.git
	runsDir  = "todotriage-runs"   // worktrees: /tmp/todotriage-runs/{run-id}
)

func repoDir(owner, repo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, reposDir, owner, repo+".git"), nil
}

func runDir(runID string) string {
	return filepath.Join(os.TempDir(), runsDir, runID)
}

// MakeRunCwd creates a throwaway cwd for delegated runs that have no worktree
// (e.g. Jira tasks with no matched repo). Lives under the same runs base as
// real worktrees so the existing Cleanup() sweep catches orphans.
//
// Giving every run a unique disposable cwd means the child claude's session
// history lands in a ~/.claude/projects/<encoded> we can cleanly delete after
// the run, rather than mixing into the parent binary's own project dir.
func MakeRunCwd(runID string) (string, error) {
	dir := filepath.Join(os.TempDir(), runsDir, runID+"-nocwd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir run cwd: %w", err)
	}
	return dir, nil
}

// RemoveRunCwd removes the throwaway cwd created by MakeRunCwd. Safe if missing.
func RemoveRunCwd(runID string) {
	os.RemoveAll(filepath.Join(os.TempDir(), runsDir, runID+"-nocwd"))
}

// RemoveClaudeProjectDir deletes the ~/.claude/projects/<encoded-cwd> entry that
// Claude Code auto-creates whenever it's invoked in a new cwd. Called after
// each delegated run to prevent a ghost project dir from accumulating for every
// ephemeral worktree path.
//
// Safety rail: only touches entries whose cwd resolves under $TMPDIR, so a
// misuse can never nuke a real project's interactive session history.
func RemoveClaudeProjectDir(cwd string) {
	if cwd == "" {
		return
	}

	// Claude Code records the symlink-resolved path
	// (e.g. /var/folders/... → /private/var/folders/... on macOS), so we need
	// the same resolution to compute the right encoded name.
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return
	}

	tmpResolved, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return
	}
	if !strings.HasPrefix(resolved, tmpResolved) {
		log.Printf("[worktree] refusing to clean project dir for non-tmp cwd: %s", resolved)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	// Claude Code encoding: replace every '/' in the absolute path with '-'.
	// The leading '/' becomes a leading '-', matching the dir names Claude Code writes.
	encoded := strings.ReplaceAll(resolved, "/", "-")
	projectDir := filepath.Join(home, claudeProjectsDir, encoded)
	if err := os.RemoveAll(projectDir); err != nil {
		log.Printf("[worktree] remove ghost project dir %s: %v", projectDir, err)
	}
}

// ensureBareClone creates a bare clone if it doesn't exist yet.
// Must be called under the per-repo lock.
func ensureBareClone(ctx context.Context, owner, repo, cloneURL string) (string, error) {
	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}

	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		log.Printf("[worktree] cloning %s/%s (first time)...", owner, repo)
		if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
			return "", fmt.Errorf("mkdir: %w", err)
		}
		start := time.Now()
		if err := gitRunCtx(ctx, "", "clone", "--bare", "--filter=blob:none", cloneURL, bareDir); err != nil {
			return "", fmt.Errorf("bare clone: %w", err)
		}
		log.Printf("[worktree] clone %s/%s completed in %s", owner, repo, time.Since(start).Round(time.Millisecond))
	}

	return bareDir, nil
}

// makeWorktreeDir creates the run directory for a worktree.
func makeWorktreeDir(runID string) (string, error) {
	wtDir := runDir(runID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir runs: %w", err)
	}
	return wtDir, nil
}

// CreateForPR sets up a worktree on the PR's head branch.
// The agent can push commits to the branch. Fetches the PR ref and checks out
// the head branch (not detached) so git push works.
func CreateForPR(ctx context.Context, owner, repo, cloneURL, headBranch string, prNumber int, runID string) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareClone(ctx, owner, repo, cloneURL)
	if err != nil {
		return "", err
	}

	// Fetch the PR's head branch
	branchRef := fmt.Sprintf("+refs/heads/%s:refs/heads/%s", headBranch, headBranch)
	start := time.Now()
	if err := gitRunCtx(ctx, bareDir, "fetch", "origin", branchRef); err != nil {
		return "", fmt.Errorf("fetch PR branch %s: %w", headBranch, err)
	}
	log.Printf("[worktree] fetch PR #%d branch %s completed in %s", prNumber, headBranch, time.Since(start).Round(time.Millisecond))

	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}

	if err := gitRunCtx(ctx, bareDir, "worktree", "add", wtDir, "refs/heads/"+headBranch); err != nil {
		return "", fmt.Errorf("worktree add: %w", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		// The worktree has already been registered with the bare repo at
		// this point, so we must roll it back before returning — otherwise
		// the caller sees an error AND has no handle to clean up with,
		// leaking a half-configured worktree directory. Remove handles
		// both the directory removal and the bare-repo worktree prune.
		if rmErr := Remove(runID); rmErr != nil {
			log.Printf("[worktree] rollback after exclude-write failure: %v", rmErr)
		}
		return "", fmt.Errorf("write local git excludes: %w", err)
	}

	log.Printf("[worktree] PR worktree at %s (branch: %s)", wtDir, headBranch)
	return wtDir, nil
}

// CreateForBranch sets up a worktree on a new feature branch based off a given base.
// If baseBranch is empty, the repo's default branch is detected from origin/HEAD.
func CreateForBranch(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID string) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareClone(ctx, owner, repo, cloneURL)
	if err != nil {
		return "", err
	}

	// Fetch the base branch
	if baseBranch == "" {
		baseBranch = detectDefaultBranch(ctx, bareDir)
	}
	baseRef := fmt.Sprintf("+refs/heads/%s:refs/heads/%s", baseBranch, baseBranch)
	start := time.Now()
	if err := gitRunCtx(ctx, bareDir, "fetch", "origin", baseRef); err != nil {
		return "", fmt.Errorf("fetch base branch %s: %w", baseBranch, err)
	}
	log.Printf("[worktree] fetch %s completed in %s", baseBranch, time.Since(start).Round(time.Millisecond))

	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}

	// Create worktree — reuse the branch if it already exists (re-delegation),
	// otherwise create a new one off the base.
	if branchExists(bareDir, featureBranch) {
		// Branch exists from a previous run — check it out
		if err := gitRunCtx(ctx, bareDir, "worktree", "add", wtDir, featureBranch); err != nil {
			return "", fmt.Errorf("worktree add existing branch: %w", err)
		}
	} else {
		if err := gitRunCtx(ctx, bareDir, "worktree", "add", "-b", featureBranch, wtDir, "refs/heads/"+baseBranch); err != nil {
			return "", fmt.Errorf("worktree add new branch: %w", err)
		}
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		// Same rollback rationale as in CreateForPR: the worktree is
		// already registered and on disk, so we own the cleanup if any
		// post-add step fails. Remove handles both the directory and the
		// bare-repo prune.
		if rmErr := Remove(runID); rmErr != nil {
			log.Printf("[worktree] rollback after exclude-write failure: %v", rmErr)
		}
		return "", fmt.Errorf("write local git excludes: %w", err)
	}

	log.Printf("[worktree] branch worktree at %s (%s from %s)", wtDir, featureBranch, baseBranch)
	return wtDir, nil
}

// managedExcludePatterns are the gitignore patterns writeLocalExcludes
// ensures are present in .git/info/exclude for every delegated worktree.
//
// - _scratch/    — CI log archives, other ephemeral download targets (SKY-146)
// - task_memory/ — cross-run structured audit entries (SKY-141)
var managedExcludePatterns = []string{"_scratch/", "task_memory/"}

// Markers delimiting the managed section of .git/info/exclude. writeLocalExcludes
// rewrites the content between these markers in place when both are present,
// and appends a fresh marker block otherwise. Using explicit markers means
// the managed section remains a self-contained complete manifest of our
// patterns regardless of how managedExcludePatterns evolves — growing the
// list reuses the existing section instead of appending a second header.
const (
	managedExcludeBegin = "# todotriage: begin managed exclude block (do not edit)"
	managedExcludeEnd   = "# todotriage: end managed exclude block"
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
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "HEAD")
	cmd.Dir = bareDir
	out, err := cmd.Output()
	if err == nil {
		// Output is like "refs/heads/main\n"
		ref := strings.TrimSpace(string(out))
		if strings.HasPrefix(ref, "refs/heads/") {
			return ref[len("refs/heads/"):]
		}
	}
	return "main"
}

// Remove cleans up a worktree after a run completes or fails.
func Remove(runID string) error {
	wtDir := runDir(runID)
	if err := os.RemoveAll(wtDir); err != nil {
		return fmt.Errorf("remove worktree dir: %w", err)
	}

	// Prune stale worktree refs from all bare repos
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pruneAll(filepath.Join(home, reposDir))

	log.Printf("[worktree] removed %s", runID)
	return nil
}

// Cleanup removes all orphaned worktrees on startup and prunes bare repos.
// Also sweeps ~/.claude/projects ghost entries for each orphaned cwd.
func Cleanup() {
	runsBase := filepath.Join(os.TempDir(), runsDir)
	entries, err := os.ReadDir(runsBase)
	if err != nil {
		return // no runs dir, nothing to clean
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() {
			fullPath := filepath.Join(runsBase, e.Name())
			// Each entry here was a live claude cwd at some point — nuke its
			// ghost ~/.claude/projects entry before removing the dir itself
			// (EvalSymlinks needs the dir to still exist to resolve).
			RemoveClaudeProjectDir(fullPath)
			os.RemoveAll(fullPath)
			count++
		}
	}

	if count > 0 {
		log.Printf("[worktree] cleaned up %d orphaned worktrees", count)
	}

	// Prune all bare repos
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	pruneAll(filepath.Join(home, reposDir))
}

func pruneAll(baseDir string) {
	if err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasSuffix(path, ".git") {
			if err := gitRun(path, "worktree", "prune"); err != nil {
				log.Printf("[worktree] prune %s: %v", path, err)
			}
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		log.Printf("[worktree] walk %s: %v", baseDir, err)
	}
}

func gitRunCtx(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func gitRun(dir string, args ...string) error {
	return gitRunCtx(context.Background(), dir, args...)
}
