package worktree

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	reposDir = ".todotinder/repos" // bare clones: ~/.todotinder/repos/{owner}/{repo}.git
	runsDir  = "todotinder-runs"   // worktrees: /tmp/todotinder-runs/{run-id}
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

// Create sets up an isolated worktree for an agent run.
// Ensures a bare clone exists, fetches latest, and creates a worktree at the given SHA.
// Returns the worktree path.
func Create(owner, repo, cloneURL, sha, runID string) (string, error) {
	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}

	// Bare clone on first use
	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		log.Printf("[worktree] cloning %s/%s (first time, may take a moment)...", owner, repo)
		if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
			return "", fmt.Errorf("mkdir: %w", err)
		}
		if err := gitRun("", "clone", "--bare", cloneURL, bareDir); err != nil {
			return "", fmt.Errorf("bare clone: %w", err)
		}
	}

	// Fetch latest refs including PR heads
	if err := gitRun(bareDir, "fetch", "origin", "--prune"); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	// Also fetch PR refs so we can check out PR head SHAs
	if err := gitRun(bareDir, "fetch", "origin", "+refs/pull/*/head:refs/pull/*/head"); err != nil {
		return "", fmt.Errorf("fetch PR refs: %w", err)
	}

	// Create worktree at target SHA
	wtDir := runDir(runID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir runs: %w", err)
	}

	if err := gitRun(bareDir, "worktree", "add", "--detach", wtDir, sha); err != nil {
		return "", fmt.Errorf("worktree add: %w", err)
	}

	log.Printf("[worktree] created at %s (sha: %s)", wtDir, sha[:12])
	return wtDir, nil
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

func gitRun(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}
