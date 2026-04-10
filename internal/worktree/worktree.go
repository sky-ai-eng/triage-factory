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

// Per-repo mutexes prevent concurrent fetches from racing on the same bare repo.
var (
	repoMu   sync.Mutex
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

	log.Printf("[worktree] branch worktree at %s (%s from %s)", wtDir, featureBranch, baseBranch)
	return wtDir, nil
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
func Cleanup() {
	runsBase := filepath.Join(os.TempDir(), runsDir)
	entries, err := os.ReadDir(runsBase)
	if err != nil {
		return // no runs dir, nothing to clean
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() {
			os.RemoveAll(filepath.Join(runsBase, e.Name()))
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
	filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
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
	})
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
