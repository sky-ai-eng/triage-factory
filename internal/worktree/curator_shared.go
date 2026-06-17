package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// sharedReaders tracks, per shared worktree path, how many sandboxed Curator
// sessions are currently reading it. The refresh gate in
// EnsureSharedCuratorWorktree resets-hard a worktree ONLY when this count is
// zero, so the host never mutates a tree that a live jail is mid-read on. The
// count is process-global (one TF process per pod — multi-pod sharding is
// TFAC-71), reset to zero on restart, which is correct: no jail survives a
// restart, so the first post-restart Acquire safely refreshes.
var (
	sharedReadersMu sync.Mutex
	sharedReaders   = map[string]int{}
)

func sharedReadersCount(wtDir string) int {
	sharedReadersMu.Lock()
	defer sharedReadersMu.Unlock()
	return sharedReaders[wtDir]
}

func sharedReadersInc(wtDir string) {
	sharedReadersMu.Lock()
	defer sharedReadersMu.Unlock()
	sharedReaders[wtDir]++
}

func sharedReadersDec(wtDir string) {
	sharedReadersMu.Lock()
	defer sharedReadersMu.Unlock()
	if sharedReaders[wtDir] <= 1 {
		delete(sharedReaders, wtDir)
		return
	}
	sharedReaders[wtDir]--
}

// EnsureSharedCuratorWorktree materializes (or reuses) the single SHARED
// read-only worktree for (orgID, owner, repo) at
// paths.CuratorSharedRepoDir(orgID, owner, repo), returning its absolute path
// plus a release func the caller MUST call once the reading jail has exited.
//
// This is the multi-mode counterpart to EnsureCuratorWorktree: instead of a
// per-project worktree under the project's knowledge dir (which a sandbox
// would chown + duplicate per session), there is exactly one on-disk checkout
// per (org, repo), bind-mounted read-only into every member's jail. N
// concurrent sessions on the same repo therefore share ~1x storage with full
// gVisor isolation (TFAC-61).
//
// Refresh model (host-side, live-reader-safe). The worktree is reset hard to
// upstream HEAD on the user-configured branch ONLY on the 0→1 reader
// transition — i.e. when no jail currently has it mounted. While at least one
// reader holds it, the tree is served as-is (a consistent snapshot) and is NOT
// reset, because a `reset --hard` / `clean` under a live reader would tear the
// files out from under an agent mid-read. This is the "refresh-between-turns"
// discipline: concurrent same-repo sessions share one consistent checkout, and
// the next refresh lands once the tree goes quiescent. Detached HEAD (not a
// local branch) is used so multiple worktrees derived from the one shared bare
// — across projects, and across orgs while the bare cache stays org-global
// pre-SKY-406 — never collide on "branch already checked out".
//
// Bind-mount readability: the returned tree is left owned by the host TF
// process (no per-session chown — chowning a shared tree to one session's
// sandbox UID is wrong). git checks files out world-readable under the
// process umask, and gVisor enforces the guest permission check against the
// file mode, so the sandbox UID reads it through the "other" bits. The mount
// is added read-only by agentproc, which is what makes an in-jail write fail.
//
// Returns a non-nil release on success only; every error path returns a nil
// release and the caller skips it. The clone seeding + auth options match
// EnsureCuratorWorktree (WithCloneURL seeds a missing bare on demand;
// WithCloneAuth carries the multi-mode App token).
func EnsureSharedCuratorWorktree(ctx context.Context, orgID, owner, repo, branch string, opts ...CloneOption) (string, func(), error) {
	cfg := resolveCloneOptions(opts)
	auth := cfg.auth
	if orgID == "" {
		return "", nil, fmt.Errorf("ensure shared curator worktree: orgID required")
	}
	if owner == "" || repo == "" {
		return "", nil, fmt.Errorf("ensure shared curator worktree: owner/repo required")
	}
	if branch == "" {
		return "", nil, fmt.Errorf("ensure shared curator worktree: branch required (caller resolves base_branch || default_branch)")
	}
	// Pre-flight the state root so a missing $HOME surfaces the actionable
	// error rather than letting paths.CuratorSharedRepoDir panic.
	if _, err := paths.StateRootErr(); err != nil {
		return "", nil, fmt.Errorf("resolve state root: %w", err)
	}
	wtDir := paths.CuratorSharedRepoDir(orgID, owner, repo)

	// The per-repo bare lock serializes every git op against the shared bare
	// (fetch, worktree add) across all callers, including concurrent Acquires
	// for the same (owner, repo). Holding it across the count-check + refresh +
	// increment makes "reset only when readers==0, then become a reader"
	// atomic: no second Acquire can slip in and add a reader between the check
	// and the reset (it would block on this lock), and no reset can run
	// concurrently with another.
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", nil, fmt.Errorf("resolve repo dir: %w", err)
	}

	remoteRef := "refs/remotes/origin/" + branch
	readers := sharedReadersCount(wtDir)

	// readers > 0 implies the worktree already exists (a reader can only be
	// registered after a successful materialization below). Serve the current
	// snapshot untouched — no fetch, no reset — so live readers see a stable
	// tree, and just account the read for the cache LRU.
	if readers > 0 {
		touchBare(bareDir)
		sharedReadersInc(wtDir)
		return wtDir, func() { sharedReadersDec(wtDir) }, nil
	}

	// readers == 0: safe to refresh. Ensure the bare exists (seed on demand,
	// mirroring EnsureCuratorWorktree), then fetch into the remote-tracking ref.
	if _, err := os.Stat(bareDir); err != nil {
		if !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("stat bare: %w", err)
		}
		if cfg.seedURL == "" {
			return "", nil, fmt.Errorf("bare clone for %s/%s missing and no clone URL provided to seed it", owner, repo)
		}
		if _, err := ensureBareCloneLocked(ctx, owner, repo, cfg.seedURL, auth); err != nil {
			return "", nil, fmt.Errorf("seed bare for %s/%s: %w", owner, repo, err)
		}
	} else {
		touchBare(bareDir)
	}

	branchRefspec := fmt.Sprintf("+refs/heads/%s:%s", branch, remoteRef)
	start := time.Now()
	if err := gitRunCtxAuth(ctx, bareDir, auth, "fetch", "origin", branchRefspec); err != nil {
		return "", nil, fmt.Errorf("fetch %s/%s %s: %w", owner, repo, branch, err)
	}
	worktreeLog.Debug("shared curator fetch", "owner", owner, "repo", repo, "branch", branch, "duration", time.Since(start).Round(time.Millisecond))

	wtInfo, statErr := os.Stat(wtDir)
	switch {
	case statErr == nil && wtInfo.IsDir():
		// Quiescent existing worktree → reset hard to the just-fetched ref so
		// the next turn reflects upstream, then drop untracked scratch.
		// gitRunCtxAuth: reset --hard materializes deferred blobs via the bare's
		// origin promisor remote, so the lazy fetch needs the host-scoped
		// credential on a private repo.
		if err := gitRunCtxAuth(ctx, wtDir, auth, "reset", "--hard", remoteRef); err != nil {
			return "", nil, fmt.Errorf("reset --hard %s: %w", branch, err)
		}
		if err := gitRunCtx(ctx, wtDir, "clean", "-fdx"); err != nil {
			worktreeLog.Warn("shared curator clean failed", "owner", owner, "repo", repo, "error", err)
		}
	case statErr == nil && !wtInfo.IsDir():
		return "", nil, fmt.Errorf("shared worktree path %s exists but is not a directory", wtDir)
	case !os.IsNotExist(statErr):
		return "", nil, fmt.Errorf("stat shared worktree %s: %w", wtDir, statErr)
	default:
		// First materialization. --detach (not -B <branch>): the shared tree
		// is read-only and a detached HEAD reserves no branch ref, so multiple
		// worktrees from the one shared bare never trip "branch already checked
		// out". reset --hard above and `git log`/`show`/`blame` host-side all
		// work fine against a detached HEAD.
		if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
			return "", nil, fmt.Errorf("mkdir parent: %w", err)
		}
		// Prune stale registrations first. If a prior checkout was removed out
		// from under the bare (operator cleanup, or a crash mid-eviction where
		// removeRegisteredWorktrees ran but os.RemoveAll(bareDir) didn't), the
		// bare still carries an admin entry for wtDir and `worktree add` would
		// fail "already registered as a linked worktree", stranding every
		// session pinning this repo until a manual prune. Best-effort under the
		// per-repo lock — prune only drops entries whose checkout is already
		// gone, so a live sibling worktree is untouched; the add error below
		// still surfaces any real failure.
		_ = gitRunCtx(ctx, bareDir, "worktree", "prune")
		// gitRunCtxAuth so the first materialization's lazy promisor fetch
		// (deferred blobs from the blobless bare) authenticates on a private repo.
		if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", "--detach", wtDir, remoteRef); err != nil {
			return "", nil, fmt.Errorf("worktree add %s/%s %s: %w", owner, repo, branch, err)
		}
		worktreeLog.Debug("shared curator worktree", "dir", wtDir, "owner", owner, "repo", repo, "branch", branch)
	}

	sharedReadersInc(wtDir)
	return wtDir, func() { sharedReadersDec(wtDir) }, nil
}
