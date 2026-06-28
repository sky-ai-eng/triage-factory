package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CreateForPR sets up a worktree on the PR's head branch.
//
// Fetches the PR head via refs/pull/<n>/head (GitHub's server-side
// mirror of every PR's head commit, available on the upstream) into
// a local branch in the bare. This works uniformly for own-repo and
// fork PRs: refs/pull/<n>/head exists on the upstream regardless of
// whether the PR's actual branch lives in the upstream or in a fork.
// Fetching refs/heads/<headBranch> directly from origin would fail
// for fork PRs because that branch isn't on the upstream.
//
// upstreamCloneURL is the base.repo.clone_url from the PR — where
// the bare's origin points and where refs/pull/*/head lives.
// headCloneURL is the head.repo.clone_url — the fork's URL when the
// PR is from a fork, equal to upstreamCloneURL otherwise.
//
// For own-repo PRs (head URL == upstream URL): local branch is named
// <headBranch>; `git push origin <headBranch>` updates the right
// place because origin IS the upstream and <headBranch> is the same
// branch on both ends.
//
// For fork PRs: local branch is named triagefactory/pr-<n> (avoids
// collisions with own-repo branches that might share the head ref
// name across concurrent runs, AND with any literal contributor
// branch named pr-<n> — the slash-prefix namespace is reserved for
// triagefactory's synthetic refs). A bare-config remote `head-<n>`
// pointing at the fork URL is added, and the local branch's
// tracking is configured so `git push` (no remote argument) pushes
// triagefactory/pr-<n> -> the fork's <headBranch>. Agents must use
// `git push` without a remote arg for this to work; envelope.txt
// has the corresponding guidance.
//
// CleanupPRConfig should be called after the run terminates to
// remove the per-PR remote and config — they live in the bare's
// shared config and would otherwise accumulate forever.
func CreateForPR(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID string, opts ...CloneOption) (string, error) {
	auth := resolveCloneOptions(opts).auth
	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}
	return createPRWorktreeAt(ctx, owner, repo, upstreamCloneURL, headCloneURL, headBranch, prNumber, runID, wtDir, auth)
}

// CreateForPRInRoot is the lazy-materialization variant of CreateForPR: the
// PR-head worktree lands at filepath.Join(runRoot, owner, repo) so a single
// run can host it as a sibling under the shared run-root (the `workspace add
// --pr` path), rather than at the run-dir CreateForPR uses for the eager
// one-repo GitHub PR delegation. Other than the path — and the absence of a
// host clone credential, matching CreateForBranchInRoot — behavior is
// identical: same fork / own-repo / deleted-fork handling, same push-tracking
// config. The run-root must already exist (created by MakeRunRoot in the
// spawner); the owner-level subdir is created here.
func CreateForPRInRoot(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID, runRoot string) (string, error) {
	if runRoot == "" {
		return "", fmt.Errorf("CreateForPRInRoot: runRoot is required")
	}
	wtDir := filepath.Join(runRoot, owner, repo)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir owner subdir: %w", err)
	}
	// No CloneAuth: this is the in-sandbox `workspace add --pr` path, where
	// in-sandbox git credentials are SKY-394's concern, not the host-side clone
	// path — the same rationale as CreateForBranchInRoot.
	return createPRWorktreeAt(ctx, owner, repo, upstreamCloneURL, headCloneURL, headBranch, prNumber, runID, wtDir, CloneAuth{})
}

// createPRWorktreeAt is the shared body of CreateForPR / CreateForPRInRoot —
// bare-clone setup, refs/pull/<n>/head fetch, `git worktree add`, the fork /
// own-repo / deleted-fork push-tracking config, and exclude-or-rollback. The
// two public callers differ only in where wtDir lives on disk and whether a
// host clone credential is threaded; wtDir's parent is created by the caller.
func createPRWorktreeAt(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID, wtDir string, auth CloneAuth) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, upstreamCloneURL, auth)
	if err != nil {
		return "", err
	}

	// GitHub can return head.repo = null for deleted-fork PRs, which leaves
	// headCloneURL empty. Those PRs are still reviewable because the head can
	// be fetched from the upstream refs/pull/<n>/head ref; they are simply not
	// pushable back to the contributor branch.
	hasHeadRepo := headCloneURL != ""

	isFork := hasHeadRepo && headCloneURL != upstreamCloneURL
	localBranch := headBranch
	if isFork {
		// triagefactory/pr-<n> is namespaced under a path-prefix that
		// would only collide with a contributor's branch literally
		// named "triagefactory/pr-<n>" (extremely unlikely). A bare
		// "pr-<n>" name would have collided with any contributor
		// using "pr-42" as a real branch name on an own-repo PR,
		// silently overwriting their fetched tip and tracking config.
		localBranch = forkPRLocalBranch(prNumber)
	}

	branchRef := fmt.Sprintf("+refs/pull/%d/head:refs/heads/%s", prNumber, localBranch)
	// Mirror the fetched tip into a remote-tracking ref alongside the local
	// branch. The workspace snapshot bounds its git bundle with `--not
	// --remotes` ("commits GitHub doesn't already have"), and a bare clone
	// keeps everything under refs/heads/* — a repo that only ever hosted
	// PR-review runs would have ZERO remote-tracking refs, making that
	// exclusion empty and the bundle a full-history pack (minutes of CPU on
	// a real repo). The mirror records "GitHub already knows this tip" in
	// the namespace the bundle excludes.
	mirrorRef := fmt.Sprintf("+refs/pull/%d/head:refs/remotes/origin/%s", prNumber, localBranch)
	start := time.Now()
	// origin == upstreamCloneURL (just ensured), so the same credential that
	// authorized the clone authorizes this fetch. The fork's head is reached
	// via the upstream's refs/pull/<n>/head, so no separate fork credential
	// is needed here.
	if err := gitRunCtxAuth(ctx, bareDir, auth, "fetch", "origin", branchRef, mirrorRef); err != nil {
		return "", fmt.Errorf("fetch PR #%d head into %s: %w", prNumber, localBranch, err)
	}
	worktreeLog.Debug("fetch PR head completed", "number", prNumber, "branch", localBranch, "duration", time.Since(start).Round(time.Millisecond))

	// Pass the bare branch name (not refs/heads/<name>) so git
	// attaches the worktree to the local branch instead of going
	// detached. `git worktree add <path> refs/heads/<name>` treats
	// the ref path as a commit-ish and detaches; `git worktree add
	// <path> <name>` resolves it as a branch and attaches.
	//
	// Routed through gitRunCtxAuth (not gitRunCtx) so the blobless bare's
	// lazy promisor fetch — git materializes the working-tree blobs the
	// partial clone deferred during checkout — carries the host-scoped
	// credential. Without it that fetch is anonymous and fails on a private
	// repo. origin is the upstream just cloned/fetched with this same auth.
	if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", wtDir, localBranch); err != nil {
		// A cancelled/killed add (ctx cancel when the task closes mid-setup)
		// leaves wtDir half-built and the bare's worktrees/<runID>/locked=
		// initializing marker behind. Plain `worktree prune` skips locked
		// entries, so without this the branch stays pinned as "checked out"
		// and the next run for this PR fails its fetch. Reclaim only THIS
		// run's registration (keyed on wtDir) so we can't disturb a
		// concurrent add against the same bare.
		_ = os.RemoveAll(wtDir)
		removeWorktreeRegFor(bareDir, wtDir)
		return "", fmt.Errorf("worktree add: %w", err)
	}

	switch {
	case isFork:
		if err := configureForkPRTracking(ctx, bareDir, prNumber, localBranch, headCloneURL, headBranch); err != nil {
			// Fork tracking is part of the worktree's contract for fork
			// PRs — without it, `git push` lands in the wrong place.
			// Roll back both the worktree AND any partial shared-bare
			// config so a half-configured state isn't left for later
			// runs to inherit.
			rollbackPRSetupLocked(ctx, bareDir, wtDir, runID, headBranch, prNumber)
			return "", fmt.Errorf("configure fork PR tracking: %w", err)
		}
	case hasHeadRepo:
		// Real own-repo PR (head URL == upstream URL). Configure
		// tracking so `git push` (no remote argument) works, matching
		// envelope guidance. With tracking unset, `git push` errors
		// with "no upstream branch" and forces agents to fall back to
		// `git push origin <branch>` — which is exactly the form we
		// discourage because it's wrong for fork PRs and a bug magnet
		// across the codebase.
		if err := configureOwnRepoPRTracking(ctx, bareDir, localBranch, prNumber); err != nil {
			rollbackPRSetupLocked(ctx, bareDir, wtDir, runID, headBranch, prNumber)
			return "", fmt.Errorf("configure own-repo PR tracking: %w", err)
		}
	default:
		// Deleted-fork PR (head.repo == null). The PR is reviewable
		// — refs/pull/<n>/head still exists on the upstream — but
		// there's no contributor remote to push back to. Skip tracking
		// so `git push` (no args) errors with "no upstream branch" and
		// surfaces the un-pushable state cleanly. Configuring origin
		// as the push target here would silently push to upstream:
		// refs/heads/<headBranch>, creating a stray branch that has
		// nothing to do with the closed PR.
		worktreeLog.Warn("PR head repository unavailable (deleted fork); worktree is read-only", "number", prNumber)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		// Tracking + remote already configured for fork/own-repo;
		// roll back both the worktree AND that shared-bare state.
		// Using rollbackPRSetupLocked instead of addExcludesOrRollback
		// here keeps the fork/own-repo cases consistent — earlier
		// rollbacks already clean up shared config, so this one
		// shouldn't be the odd path that leaves it behind.
		rollbackPRSetupLocked(ctx, bareDir, wtDir, runID, headBranch, prNumber)
		return "", fmt.Errorf("write local git excludes: %w", err)
	}

	worktreeLog.Info("PR worktree created", "dir", wtDir, "branch", localBranch, "head", headBranch, "fork", isFork)
	return wtDir, nil
}

// rollbackPRSetupLocked undoes a partially-set-up PR worktree:
// removes the worktree directory + bare's worktree registration,
// then removes any shared-bare config that earlier steps already
// wrote (head-<n> remote, branch.triagefactory/pr-<n>.* tracking,
// branch.<headBranch>.* tracking, and the synthetic local branch).
//
// Caller must hold the per-repo lock (CreateForPR's mu). Best-effort
// — individual command failures are logged but don't propagate. The
// caller still returns the original setup error.
//
// Without this, a fork-tracking failure mid-write would leave a
// stray head-<n> remote and partial branch tracking in the bare;
// SweepStaleForkPRConfig would eventually clean those up on next
// bootstrap, but until then they'd sit in the config potentially
// confusing later runs.
func rollbackPRSetupLocked(ctx context.Context, bareDir, wtDir, runID, headBranch string, prNumber int) {
	if rmErr := RemoveAt(wtDir, runID); rmErr != nil {
		worktreeLog.Warn("rollback PR setup: remove worktree failed", "error", rmErr)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	removePRConfigLocked(cleanupCtx, bareDir, headBranch, prNumber)
}

// forkPRLocalBranch returns the bare-local branch name we use for a
// fork PR's checkout. Centralized so CreateForPR and CleanupPRConfig
// can't drift from each other on the naming convention.
func forkPRLocalBranch(prNumber int) string {
	return fmt.Sprintf("triagefactory/pr-%d", prNumber)
}

// forkPRRemoteName returns the bare-config remote name we use for a
// fork PR's contributor remote. Per-PR rather than per-fork-owner so
// add/set-url is idempotent and stale URLs from one PR can't
// contaminate another.
func forkPRRemoteName(prNumber int) string {
	return fmt.Sprintf("head-%d", prNumber)
}

// trackedBranchMarkerKey is the per-branch config key that marks a
// branch as triagefactory-managed. We write it from both
// configureForkPRTracking and configureOwnRepoPRTracking; the sweep
// reads it via `git config --get-regexp` to identify orphaned
// branches that need cleanup, including own-repo branches the
// fork-only sweep would otherwise miss after a run's worktree is
// removed. The value is the PR number — preserved as the source
// of truth for the head-<n> remote name when one exists.
//
// Git lower-cases config variable names internally, so the regex
// in the sweep matches against `tfprnumber` even though we set
// `tfPRNumber` here. Using the lowercase form everywhere keeps the
// match logic obvious.
const trackedBranchMarkerKey = "tfprnumber"

// cleanupTimeout caps the time the detached-context PR-config cleanups
// (CleanupPRConfig, SweepStaleForkPRConfig, and rollbackPRSetupLocked) will
// spend on their git invocations. Reclamation is best-effort; if a single
// config-rewrite hangs (locked file, slow disk), we'd rather time out than
// block run finalization indefinitely.
const cleanupTimeout = 30 * time.Second

// CleanupPRConfig removes per-PR config blocks the bare repo
// accumulated for a delegated PR run: the head-<n> remote and
// triagefactory/pr-<n> branch tracking from fork-PR setup, and the
// branch.<headBranch>.* tracking from own-repo PR setup. Idempotent
// — anything already absent is a no-op.
//
// headBranch is the PR's actual head ref (cfg.headRef in the
// spawner's runConfig). For own-repo PRs it's the worktree's local
// branch; for fork PRs it's the contributor's branch on the fork.
// Pass it so we can also clean the own-repo branch.<headBranch>.*
// tracking — without it, only fork-specific config gets reclaimed.
//
// Uses a detached background context with a bounded timeout rather
// than threading the caller's ctx through. Cleanup must still run
// when the agent's parent ctx has already been cancelled (run timed
// out, user cancelled the run, server shutdown) — otherwise every
// gitRunCtx call short-circuits with the ctx error and the per-PR
// config never gets reclaimed.
//
// Should be called after the worktree has been removed (RemoveAt) so
// `git branch -D` doesn't fight with an in-use checkout. Errors from
// individual git invocations are swallowed — cleanup is best-effort
// and a partial failure shouldn't propagate up the run-finalization
// path.
func CleanupPRConfig(owner, repo, headBranch string, prNumber int) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return
	}
	if _, err := os.Stat(bareDir); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	removePRConfigLocked(ctx, bareDir, headBranch, prNumber)
}

// removePRConfigLocked is the config-removal sequence for a single
// PR. Caller must hold the per-repo lock. Used by both the inline
// CleanupPRConfig (run finalization) and SweepStaleForkPRConfig
// (bootstrap-time backstop) so the cleanup steps stay in lockstep.
//
// headBranch may be empty — pass "" when only fork-specific artifacts
// are known (e.g. when discovering an orphan via the triagefactory/
// pr-<n> branch alone). When set, both the branch tracking
// (branch.<headBranch>.*) and the local branch ref (refs/heads/
// <headBranch>) are reclaimed too — important for deleted-fork PRs
// (where the only local ref is refs/heads/<headBranch>) and to
// prevent CreateForBranch from picking up a stale tip on a future
// Jira delegation that happens to use the same branch name.
//
// Branch deletion is safe because we always call this AFTER RemoveAt
// has destroyed the worktree dir; git refuses `branch -D` for a
// branch checked out by any live worktree, so a concurrent
// delegation would force this to no-op rather than silently break a
// live checkout.
//
// All commands tolerate "already absent": git remote remove errors
// when the remote isn't there, --remove-section errors when the
// section is absent, branch -D errors when the branch is gone.
// Each error is a normal idempotent state.
func removePRConfigLocked(ctx context.Context, bareDir, headBranch string, prNumber int) {
	remoteName := forkPRRemoteName(prNumber)
	syntheticBranch := forkPRLocalBranch(prNumber)
	_ = gitRunCtx(ctx, bareDir, "remote", "remove", remoteName)
	_ = gitRunCtx(ctx, bareDir, "config", "--remove-section", "branch."+syntheticBranch)
	_ = gitRunCtx(ctx, bareDir, "branch", "-D", syntheticBranch)
	if headBranch != "" && headBranch != syntheticBranch {
		_ = gitRunCtx(ctx, bareDir, "config", "--remove-section", "branch."+headBranch)
		// Delete the local copy of the head branch the fetch refspec
		// created at refs/heads/<headBranch>. Required for deleted-fork
		// PRs (their data lives there, not at the synthetic name) and
		// for own-repo PRs to keep CreateForBranch's branchExists path
		// from reusing a stale tip when a future Jira delegation
		// happens to use the same branch name. Re-delegating the same
		// PR re-fetches refs/pull/<n>/head, so deletion is reversible.
		_ = gitRunCtx(ctx, bareDir, "branch", "-D", headBranch)
	}
}

// SweepStaleForkPRConfig walks every branch the bare has marked as
// triagefactory-managed (via the trackedBranchMarkerKey config we
// write from configureForkPRTracking and configureOwnRepoPRTracking)
// and removes any whose branch isn't currently checked out by a
// live worktree. Backstop for the cases where inline CleanupPRConfig
// in the runAgent defer doesn't fire:
//
//   - Run was cancelled at a layer above the runAgent defer (rare):
//     inline cleanup never runs.
//
// Walking markers (rather than head-<n> remotes alone) is what makes
// this cover own-repo PRs too — fork PRs have a head-<n> the old
// approach could find, but own-repo PRs have only the branch config
// block, which the marker exposes generically.
//
// Safe to call while runs are still in flight because `git worktree
// list` reports their checkouts and we only reclaim branches with no
// live checkout. Best-effort: orphan-detection failures or partial
// removes correct themselves on the next bootstrap.
func SweepStaleForkPRConfig(owner, repo string) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return
	}
	if _, err := os.Stat(bareDir); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	inUse := liveWorktreeBranches(ctx, bareDir)

	// `--get-regexp` returns "<key> <value>\n" per match and exits
	// non-zero with no output when nothing matches; tolerate that
	// here so a fresh repo with no managed branches isn't an error.
	pattern := fmt.Sprintf(`^branch\..*\.%s$`, trackedBranchMarkerKey)
	out, _ := gitOutputCtx(ctx, bareDir, "config", "--get-regexp", pattern)

	keyPrefix := "branch."
	keySuffix := "." + trackedBranchMarkerKey
	reclaimed := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Each line: `branch.<branchName>.tfprnumber <prNumber>`.
		// Branch names can contain spaces in theory, but git
		// rejects them in practice, so a single-space split between
		// key and value is safe.
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if !strings.HasPrefix(key, keyPrefix) || !strings.HasSuffix(key, keySuffix) {
			continue
		}
		branch := strings.TrimSuffix(strings.TrimPrefix(key, keyPrefix), keySuffix)
		prNumber, err := strconv.Atoi(value)
		if err != nil {
			continue
		}
		if inUse[branch] {
			continue
		}
		// Pass the marked branch as headBranch so removePRConfigLocked
		// also wipes refs/heads/<branch> and the branch.<branch>.*
		// section. The synthetic triagefactory/pr-<n> branch (if any)
		// gets cleaned by the same call's fork-specific path.
		removePRConfigLocked(ctx, bareDir, branch, prNumber)
		reclaimed++
	}
	if reclaimed > 0 {
		worktreeLog.Warn("swept stale per-PR config blocks", "count", reclaimed, "dir", bareDir)
	}
}

// liveWorktreeBranches returns the set of refs/heads/<name> that
// `git worktree list --porcelain` reports as checked out somewhere
// (the bare itself if it has a HEAD, plus every linked worktree).
// The sweep uses this to decide whether a head-<n> remote is still
// actively backing a checkout — if its triagefactory/pr-<n> branch
// is in this set, removing the remote would break that worktree.
func liveWorktreeBranches(ctx context.Context, bareDir string) map[string]bool {
	branches := make(map[string]bool)
	out, err := gitOutputCtx(ctx, bareDir, "worktree", "list", "--porcelain")
	if err != nil {
		return branches
	}
	const prefix = "branch refs/heads/"
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			branches[strings.TrimPrefix(line, prefix)] = true
		}
	}
	return branches
}

// configureForkPRTracking sets up the worktree's local branch so
// `git push` (no remote argument) sends commits to the contributor's
// fork at the right branch name. Configures four pieces:
//
//   - A bare-config remote head-<prNumber> -> forkCloneURL. Per-PR
//     naming (vs per-fork-owner) keeps add/set-url idempotent and
//     prevents stale URLs from one PR contaminating another.
//   - branch.<localBranch>.remote / .merge so `git pull` treats
//     the fork as the upstream and the agent can refresh.
//   - branch.<localBranch>.pushRemote so push specifically targets
//     the fork even if remote.pushDefault changes elsewhere.
//   - remote.<remoteName>.push as an explicit refspec mapping
//     refs/heads/<localBranch> -> refs/heads/<forkBranch>. Without
//     this, `git push` (no args) under the default push.default
//     ("simple") errors with "names don't match" because local
//     triagefactory/pr-<n> and remote <forkBranch> differ. The
//     explicit refspec bypasses the name-match check and pushes to
//     the right place.
//
// Idempotent: re-running on an already-configured state updates URLs
// and rewrites config keys to current values.
func configureForkPRTracking(ctx context.Context, bareDir string, prNumber int, localBranch, forkCloneURL, forkBranch string) error {
	remoteName := forkPRRemoteName(prNumber)

	// Add or update the fork remote. `git remote add` errors when the
	// remote already exists; fall through to set-url in that case so
	// repeat calls (re-delegation, retries) are no-ops on URL match
	// and corrective on URL drift.
	if err := gitRunCtx(ctx, bareDir, "remote", "add", remoteName, forkCloneURL); err != nil {
		if err := gitRunCtx(ctx, bareDir, "remote", "set-url", remoteName, forkCloneURL); err != nil {
			return fmt.Errorf("add or set-url remote %s: %w", remoteName, err)
		}
	}

	pushRefspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", localBranch, forkBranch)
	prMarker := strconv.Itoa(prNumber)
	cfgs := [][]string{
		{"config", fmt.Sprintf("branch.%s.remote", localBranch), remoteName},
		{"config", fmt.Sprintf("branch.%s.merge", localBranch), "refs/heads/" + forkBranch},
		{"config", fmt.Sprintf("branch.%s.pushRemote", localBranch), remoteName},
		{"config", fmt.Sprintf("remote.%s.push", remoteName), pushRefspec},
		// Marker so SweepStaleForkPRConfig can find this branch
		// generically (the head-<n> remote alone wouldn't cover
		// own-repo PRs, but the marker covers both flows).
		{"config", fmt.Sprintf("branch.%s.%s", localBranch, trackedBranchMarkerKey), prMarker},
	}
	for _, args := range cfgs {
		if err := gitRunCtx(ctx, bareDir, args...); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// configureOwnRepoPRTracking sets per-branch tracking
// (branch.<localBranch>.{remote,merge}) so `git push` (no remote
// argument) resolves to origin/<localBranch> for this specific
// branch. Since local and remote branch names match for own-repo
// PRs, push.default=simple (the default) is happy without further
// config.
//
// Per-branch rather than repo-wide intentionally. Repo-wide settings
// like remote.pushDefault and push.default=current would leak across
// every worktree off this bare: once any own-repo PR runs, a later
// deleted-fork PR (which deliberately skips tracking so push fails
// loudly) would silently resolve `git push` against the inherited
// repo-wide settings and create a stray branch on upstream. Per-PR
// config blocks accumulate, but bounded by unique head-ref names
// across the repo's lifetime — a real concern only at repo scales
// well beyond what this tool targets.
//
// Also writes the trackedBranchMarkerKey so the sweep can reclaim
// this block when the spawner's inline cleanup doesn't fire.
func configureOwnRepoPRTracking(ctx context.Context, bareDir, localBranch string, prNumber int) error {
	prMarker := strconv.Itoa(prNumber)
	cfgs := [][]string{
		{"config", fmt.Sprintf("branch.%s.remote", localBranch), "origin"},
		{"config", fmt.Sprintf("branch.%s.merge", localBranch), "refs/heads/" + localBranch},
		{"config", fmt.Sprintf("branch.%s.%s", localBranch, trackedBranchMarkerKey), prMarker},
	}
	for _, args := range cfgs {
		if err := gitRunCtx(ctx, bareDir, args...); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}
