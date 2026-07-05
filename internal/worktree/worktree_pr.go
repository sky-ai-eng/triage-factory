package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
// The bare-local branch is run-namespaced for EVERY PR checkout —
// triagefactory/<runID>/pr-<n> (prLocalBranch) — so two concurrent runs
// reviewing the same PR off one shared bare never collide on a branch ref
// (the fetch and `worktree add` would otherwise be refused). This is one
// uniform scheme for fork AND own-repo PRs; only the push target differs.
//
// Push tracking (configurePRPushTracking) wires a per-run remote
// tfpush-<runID>-<n> -> headCloneURL with an explicit push refspec
// (the run-namespaced local branch -> the real head branch), so `git
// push` (no remote argument) lands on the fork's contributor branch
// (fork PR) or the upstream head (own-repo PR). Agents must use `git
// push` without a remote arg for this to work; envelope.txt has the
// corresponding guidance. A deleted-fork PR (head.repo == null) skips
// push tracking and stays read-only.
//
// CleanupPRConfig should be called after the run terminates (with the
// SAME runID) to remove the per-run remote + branch config — they live
// in the bare's shared config and would otherwise accumulate forever.
func CreateForPR(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID string, opts ...CloneOption) (string, error) {
	cfg := resolveCloneOptions(opts)
	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}
	// Multi mode sandboxes the run, so it needs a self-contained clone whose
	// .git doesn't point back into the (unmounted) bare; local mode runs on-host
	// where the bare is reachable and keeps the zero-copy linked worktree.
	return createPRWorktreeAt(ctx, owner, repo, upstreamCloneURL, headCloneURL, headBranch, cfg.baseBranch, prNumber, runID, wtDir, cfg.auth, runmode.Current() == runmode.ModeMulti)
}

// CreateForPRInRoot is the lazy-materialization variant of CreateForPR: the
// PR-head worktree lands at filepath.Join(runRoot, owner, repo, "pr-<n>") so a
// single run can host SEVERAL per-repo / per-PR worktrees as siblings under the
// shared run-root (the `workspace add --pr` path) — the ref-slug subdir is what
// lets two PRs in one repo coexist (TFAC-502). The eager one-repo GitHub PR
// delegation uses the run-dir CreateForPR instead. Other than the path,
// behavior is identical: same fork / own-repo / deleted-fork handling, same
// per-run push-tracking config, same runmode split (local → zero-copy linked
// worktree; multi → self-contained clone, since the sandbox the run root is
// mounted into can't see the shared bare). The run-root must already exist
// (created by MakeRunRoot in the spawner); the owner/repo subdirs are created
// here.
//
// Since TFAC-546 this runs HOST-SIDE in both modes (the agenthost daemon calls
// it on the sandbox's behalf in multi), so WithCloneAuth is honored exactly as
// in CreateForPR.
func CreateForPRInRoot(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID, runRoot string, opts ...CloneOption) (string, error) {
	if runRoot == "" {
		return "", fmt.Errorf("CreateForPRInRoot: runRoot is required")
	}
	cfg := resolveCloneOptions(opts)
	wtDir := filepath.Join(runRoot, owner, repo, PRRefSlug(prNumber))
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir repo subdir: %w", err)
	}
	return createPRWorktreeAt(ctx, owner, repo, upstreamCloneURL, headCloneURL, headBranch, cfg.baseBranch, prNumber, runID, wtDir, cfg.auth, runmode.Current() == runmode.ModeMulti)
}

// createPRWorktreeAt is the shared body of CreateForPR / CreateForPRInRoot —
// bare-clone setup, refs/pull/<n>/head fetch, base-branch refresh, and the fork
// / own-repo / deleted-fork push-tracking config. The run root is materialized
// either as a linked `git worktree` (selfContained=false) or, for a sandboxed
// multi-mode run, as a self-contained clone (selfContained=true — see
// finishSelfContainedPRClone). The public callers differ in where wtDir lives on
// disk, whether a host clone credential is threaded, and that selfContained
// choice; wtDir's parent is created by the caller.
func createPRWorktreeAt(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch, baseBranch string, prNumber int, runID, wtDir string, auth CloneAuth, selfContained bool) (string, error) {
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
	// Per-run-unique bare-local branch for EVERY PR checkout — fork, own-repo,
	// and deleted-fork alike. The own-repo path used to attach the head branch
	// name itself and the fork path a per-PR triagefactory/pr-<n>; both collided
	// when two runs shared one bare (git refuses to fetch into / check out a
	// local branch live in another worktree). prLocalBranch namespaces by run_id
	// so concurrent same-PR runs never share a ref (TFAC-87/TFAC-502). The
	// fetch refspecs below inherit that uniqueness, so the fetch no longer hits
	// "refusing to fetch into branch ... checked out at ...".
	localBranch := prLocalBranch(runID, prNumber)

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

	// Refresh the base branch's remote-tracking ref so the worktree-local PR diff
	// (cmd/exec/gh) frames against a CURRENT base. The bare reuses a clone-time
	// origin/<base> across runs and the fetch above only touches the head, so
	// without this the three-dot merge-base can fall behind the PR's branch point
	// and replay already-merged commits as phantom hunks (TFAC-505). The base
	// lives on origin (the upstream) for fork and own-repo PRs alike.
	//
	// Best-effort: a deleted / renamed / unknown base branch must not fail the
	// worktree the head fetch already established — log and proceed; the diff path
	// falls back to the recorded base.sha (and then the API) when this ref is
	// stale or absent. Empty baseBranch (caller didn't supply one) skips it.
	if baseBranch != "" {
		baseTrackingRef := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", baseBranch, baseBranch)
		if err := gitRunCtxAuth(ctx, bareDir, auth, "fetch", "origin", baseTrackingRef); err != nil {
			worktreeLog.Warn("refresh PR base branch failed; diff frames against recorded base / API instead", "number", prNumber, "base", baseBranch, "error", err)
		}
	}

	// Multi-mode delegation bind-mounts the run root into a sandbox that can't
	// see the bare, so a linked worktree (whose .git points back into the bare)
	// is dead on arrival there. Materialize a self-contained clone instead — the
	// shared bare stays the object source; only this final per-run step differs.
	// Local mode keeps the zero-copy worktree (the bare is reachable on-host).
	if selfContained {
		return finishSelfContainedPRClone(ctx, bareDir, wtDir, localBranch, baseBranch, upstreamCloneURL, headCloneURL, headBranch, runID, prNumber, hasHeadRepo, isFork, auth)
	}

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
	case hasHeadRepo:
		// Fork OR own-repo PR. One uniform per-run push remote whose URL is the
		// head repo's: the fork for a fork PR, the upstream (== origin) for an
		// own-repo PR. Because the bare-local branch is now run-namespaced and
		// differs from the real head branch in both cases, an explicit push
		// refspec (local -> real head) is what makes `git push` (no remote arg)
		// land on the contributor's fork branch / the upstream head respectively.
		// Without it `git push` errors with "no upstream branch" and forces the
		// `git push origin <branch>` form we discourage (wrong for fork PRs, a
		// bug magnet across the codebase).
		if err := configurePRPushTracking(ctx, bareDir, runID, prNumber, localBranch, headCloneURL, headBranch); err != nil {
			// Push tracking is part of the worktree's contract — without it
			// `git push` lands in the wrong place. Roll back both the worktree
			// AND any partial shared-bare config so a half-configured state
			// isn't left for later runs to inherit.
			rollbackPRSetupLocked(ctx, bareDir, wtDir, runID, prNumber)
			return "", fmt.Errorf("configure PR push tracking: %w", err)
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
		rollbackPRSetupLocked(ctx, bareDir, wtDir, runID, prNumber)
		return "", fmt.Errorf("write local git excludes: %w", err)
	}

	worktreeLog.Info("PR worktree created", "dir", wtDir, "branch", localBranch, "head", headBranch, "fork", isFork)
	return wtDir, nil
}

// finishSelfContainedPRClone materializes the PR run root as a STANDALONE clone
// of the bare instead of a linked worktree. Multi-mode delegation bind-mounts
// the run root into a sandbox that can't see the bare, so a worktree's .git
// pointer (gitdir: <bare>/worktrees/...) dangles there and every git command
// fails. A standalone clone carries its own .git, so the run root is fully
// self-contained once mounted. Caller holds the per-repo lock and has already
// fetched refs/heads/<localBranch> into the bare.
//
// Two things differ from the worktree path, both forced by self-containment:
//   - the transient per-run branch is dropped from the shared bare once the
//     clone has copied its objects (the worktree path keeps it — its live
//     checkout needs it);
//   - push tracking is written into the CLONE's config, not the bare's, so the
//     `git push` wiring travels into the sandbox with the run root.
func finishSelfContainedPRClone(ctx context.Context, bareDir, wtDir, localBranch, baseBranch, upstreamCloneURL, headCloneURL, headBranch, runID string, prNumber int, hasHeadRepo, isFork bool, auth CloneAuth) (string, error) {
	if err := materializeSelfContainedClone(ctx, bareDir, wtDir, localBranch, baseBranch, upstreamCloneURL, auth); err != nil {
		_ = os.RemoveAll(wtDir)
		dropBareRunRefs(ctx, bareDir, localBranch)
		return "", err
	}
	// The clone owns the objects it needs now; drop the per-run branch and its
	// mirror from the shared bare so it accumulates no per-run refs.
	dropBareRunRefs(ctx, bareDir, localBranch)

	switch {
	case hasHeadRepo:
		// Fork OR own-repo PR — identical wiring to the worktree path, only the
		// git dir differs: the config lives in the CLONE (wtDir) so it ships into
		// the sandbox with the run root.
		if err := configurePRPushTracking(ctx, wtDir, runID, prNumber, localBranch, headCloneURL, headBranch); err != nil {
			_ = os.RemoveAll(wtDir)
			return "", fmt.Errorf("configure PR push tracking: %w", err)
		}
	default:
		// Deleted-fork PR (head.repo == null): reviewable but not pushable. No
		// push remote, so `git push` fails loudly (read-only, by design).
		worktreeLog.Warn("PR head repository unavailable (deleted fork); run clone is read-only", "number", prNumber)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		_ = os.RemoveAll(wtDir)
		return "", fmt.Errorf("write local git excludes: %w", err)
	}

	worktreeLog.Info("PR run clone created (self-contained)", "dir", wtDir, "branch", localBranch, "head", headBranch, "fork", isFork)
	return wtDir, nil
}

// materializeSelfContainedClone builds a standalone git clone at wtDir from the
// shared bare, checked out at branch (which must already exist as a local branch
// in the bare). The bare stays the object source — one remote fetch, reused
// across runs — and only this per-run copy is new; the bare's blob:none filter
// keeps that copy small.
//
// origin is repointed from the bare's on-disk path (invisible in the sandbox) to
// the upstream and marked a blobless promisor, so the checkout's deferred-blob
// materialization and any later in-sandbox fetch/push resolve through the
// gitproxy against GitHub.
func materializeSelfContainedClone(ctx context.Context, bareDir, wtDir, branch, baseBranch, upstreamCloneURL string, auth CloneAuth) error {
	// Local object COPY (--no-hardlinks): the bare (/data) and the run root
	// (/tmp, ephemeral) sit on different filesystems, so hardlinks can't span
	// them. --no-checkout defers the working tree until the promisor is wired,
	// so the deferred-blob fetch has an upstream to resolve against.
	if err := gitRunCtx(ctx, "", "clone", "--no-hardlinks", "--no-checkout", "--single-branch", "--branch", branch, bareDir, wtDir); err != nil {
		return fmt.Errorf("clone run root from bare: %w", err)
	}

	if err := gitRunCtx(ctx, wtDir, "remote", "set-url", "origin", upstreamCloneURL); err != nil {
		return fmt.Errorf("point clone origin at upstream: %w", err)
	}
	for _, kv := range [][2]string{
		{"remote.origin.promisor", "true"},
		{"remote.origin.partialclonefilter", "blob:none"},
	} {
		if err := gitRunCtx(ctx, wtDir, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("configure clone promisor %s: %w", kv[0], err)
		}
	}

	// Materialize the working tree. auth authenticates the promisor's
	// deferred-blob fetch on a private repo — the GIT_CONFIG_* env carrying the
	// header is inherited by the child fetch git, as in the worktree path.
	if err := gitRunCtxAuth(ctx, wtDir, auth, "checkout", branch); err != nil {
		return fmt.Errorf("checkout %s in run clone: %w", branch, err)
	}

	// Bring the base branch's tracking ref into the clone for diff framing.
	// createPRWorktreeAt refreshed the bare's refs/remotes/origin/<base> from
	// upstream just before the clone, and the local clone copied the bare's whole
	// object store — so base's objects are already here and only the ref is
	// missing (a --single-branch clone doesn't carry the source's remote-tracking
	// refs). Copy it in locally rather than paying a second network fetch for a
	// tip the bare already has. Best-effort: if the bare fetch didn't land the
	// ref, the diff path falls back to the recorded base sha / API.
	if baseBranch != "" {
		baseRef := "refs/remotes/origin/" + baseBranch
		if sha, err := gitOutputCtx(ctx, bareDir, "rev-parse", "--verify", baseRef); err == nil {
			if err := gitRunCtx(ctx, wtDir, "update-ref", baseRef, strings.TrimSpace(sha)); err != nil {
				worktreeLog.Warn("copy base branch ref into run clone failed; diff frames against recorded base / API", "base", baseBranch, "error", err)
			}
		}
	}
	return nil
}

// dropBareRunRefs removes a per-run PR branch and its remote-tracking mirror
// from the shared bare. Best-effort — both git calls tolerate an already-absent
// ref. The self-contained path calls this once its standalone clone has copied
// the objects, so the shared bare is left with no per-run refs.
func dropBareRunRefs(ctx context.Context, bareDir, localBranch string) {
	_ = gitRunCtx(ctx, bareDir, "branch", "-D", localBranch)
	_ = gitRunCtx(ctx, bareDir, "update-ref", "-d", "refs/remotes/origin/"+localBranch)
}

// rollbackPRSetupLocked undoes a partially-set-up PR worktree:
// removes the worktree directory + bare's worktree registration,
// then removes any shared-bare config that earlier steps already
// wrote (the per-run tfpush-<runID>-<n> remote, the per-run
// branch.triagefactory/<runID>/pr-<n>.* tracking, and the synthetic
// local branch).
//
// Caller must hold the per-repo lock (CreateForPR's mu). Best-effort
// — individual command failures are logged but don't propagate. The
// caller still returns the original setup error.
//
// Without this, a push-tracking failure mid-write would leave a
// stray per-run remote and partial branch tracking in the bare;
// SweepStaleForkPRConfig would eventually clean those up on next
// bootstrap, but until then they'd sit in the config potentially
// confusing later runs.
func rollbackPRSetupLocked(ctx context.Context, bareDir, wtDir, runID string, prNumber int) {
	if rmErr := RemoveAt(wtDir, runID); rmErr != nil {
		worktreeLog.Warn("rollback PR setup: remove worktree failed", "error", rmErr)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	removePRConfigLocked(cleanupCtx, bareDir, prLocalBranch(runID, prNumber), prNumber)
}

// prLocalBranch returns the bare-local branch name a PR checkout attaches in
// the shared bare. Namespaced by run_id so two concurrent runs reviewing the
// SAME PR (sharing one bare) never share a branch ref — git refuses to fetch
// into, or `worktree add`, a local branch already checked out in another live
// worktree of the same bare, so a per-PR-only name (the old triagefactory/pr-N)
// would make the second run's materialization fail (TFAC-87/TFAC-502). One
// uniform scheme for fork AND own-repo PRs (own-repo used to reuse the head
// branch name itself, which collided the same way). The triagefactory/ prefix
// reserves the namespace from any literal contributor branch. Centralized so
// CreateForPR, the push-tracking config, and the cleanup paths can't drift on
// the convention.
func prLocalBranch(runID string, prNumber int) string {
	return fmt.Sprintf("triagefactory/%s/pr-%d", runID, prNumber)
}

// parsePRLocalBranch reverses prLocalBranch, recovering the run id from a
// per-run PR branch name (triagefactory/<runID>/pr-<N>). The sweep walks branch
// markers — which carry the branch name, not the run id — so it needs this to
// reconstruct the per-run push remote for reclamation. ok=false for any branch
// that isn't one of ours (so the sweep skips it safely).
func parsePRLocalBranch(branch string) (runID string, prNumber int, ok bool) {
	rest, ok := strings.CutPrefix(branch, "triagefactory/")
	if !ok {
		return "", 0, false
	}
	cut := strings.LastIndex(rest, "/pr-")
	if cut <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[cut+len("/pr-"):])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return rest[:cut], n, true
}

// prPushRemoteName returns the bare-config remote name a PR worktree pushes
// through. Per (run, PR) — not per-PR — so two concurrent runs on the same PR
// each own their own remote: inline cleanup of one run can then reclaim its
// remote without yanking it out from under the other (a single shared per-PR
// remote would break the surviving run). The runID/pr namespace is flattened
// (remote names can't contain '/'). Derivable from the per-run branch alone via
// parsePRLocalBranch, so the bootstrap sweep can reconstruct it for orphans.
func prPushRemoteName(runID string, prNumber int) string {
	return fmt.Sprintf("tfpush-%s-%d", runID, prNumber)
}

// PRRefSlug is the run_worktrees ref and worktree-subdir name for a PR-head
// checkout: "pr-<N>". Exported so the workspace CLI reserves the row and
// computes the worktree path with the same slug the InRoot create uses.
func PRRefSlug(prNumber int) string {
	return fmt.Sprintf("pr-%d", prNumber)
}

// trackedBranchMarkerKey is the per-branch config key that marks a
// branch as triagefactory-managed. configurePRPushTracking writes it on
// every per-run PR branch (triagefactory/<runID>/pr-<n>, fork and
// own-repo alike); the sweep reads it via `git config --get-regexp` to
// find orphaned branches that need cleanup after a run's worktree is
// gone. The value is the PR number — preserved as the source of truth
// for reconstructing the per-run push remote (tfpush-<runID>-<n>) during
// reclamation.
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

// CleanupPRConfig removes the per-PR config a bare repo accumulated for
// one delegated PR run: the per-run tfpush-<runID>-<n> push remote, the
// per-run branch.triagefactory/<runID>/pr-<n>.* tracking, the local
// branch ref itself, and its remote-tracking mirror. Idempotent —
// anything already absent is a no-op.
//
// runID identifies the finishing run so it reclaims ITS OWN per-run
// branch and remote, never a sibling's: a second concurrent run on the
// same PR has a distinct run-namespaced branch/remote that this call
// leaves untouched. No head branch is needed — every PR checkout (fork,
// own-repo, deleted-fork) now lives under the run-namespaced local
// branch, so there is no separate refs/heads/<headBranch> copy to clean.
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
func CleanupPRConfig(owner, repo string, prNumber int, runID string) {
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
	removePRConfigLocked(ctx, bareDir, prLocalBranch(runID, prNumber), prNumber)
}

// removePRConfigLocked is the config-removal sequence for a single PR
// worktree, keyed on its per-run bare-local branch. Caller must hold the
// per-repo lock. Used by both the inline CleanupPRConfig (run
// finalization) and SweepStaleForkPRConfig (bootstrap-time backstop) so
// the cleanup steps stay in lockstep.
//
// localBranch is the run-namespaced branch (triagefactory/<runID>/pr-<n>):
// the inline path computes it from (runID, prNumber); the sweep already
// has it verbatim from the branch marker it walked. The per-run push
// remote is reconstructed from it via parsePRLocalBranch.
//
// Branch deletion is safe because we always call this AFTER RemoveAt
// has destroyed the worktree dir; git refuses `branch -D` for a
// branch checked out by any live worktree, so a concurrent
// delegation (a second run on the same PR has a DIFFERENT branch
// anyway) would force this to no-op rather than break a live checkout.
//
// All commands tolerate "already absent": git remote remove errors
// when the remote isn't there, --remove-section errors when the
// section is absent, branch -D / update-ref -d error when the ref is
// gone. Each error is a normal idempotent state.
func removePRConfigLocked(ctx context.Context, bareDir, localBranch string, prNumber int) {
	if runID, _, ok := parsePRLocalBranch(localBranch); ok {
		_ = gitRunCtx(ctx, bareDir, "remote", "remove", prPushRemoteName(runID, prNumber))
	}
	_ = gitRunCtx(ctx, bareDir, "config", "--remove-section", "branch."+localBranch)
	_ = gitRunCtx(ctx, bareDir, "branch", "-D", localBranch)
	// Drop the remote-tracking mirror the fetch refspec created alongside the
	// local branch (refs/remotes/origin/<localBranch>) so a finishing run leaves
	// no per-run ref behind. update-ref -d no-ops when it's already gone.
	_ = gitRunCtx(ctx, bareDir, "update-ref", "-d", "refs/remotes/origin/"+localBranch)
}

// SweepStaleForkPRConfig walks every branch the bare has marked as
// triagefactory-managed (via the trackedBranchMarkerKey config
// configurePRPushTracking writes) and removes any whose branch isn't
// currently checked out by a live worktree. Backstop for the cases where
// inline CleanupPRConfig in the runAgent defer doesn't fire:
//
//   - Run was cancelled at a layer above the runAgent defer (rare):
//     inline cleanup never runs.
//
// Walking markers (rather than per-run push remotes alone) is what
// makes this cover every PR worktree uniformly: each marked branch is a
// run-namespaced triagefactory/<runID>/pr-<n>, and removePRConfigLocked
// reconstructs the matching per-run push remote from the branch name.
//
// Safe to call while runs are still in flight because `git worktree
// list` reports their checkouts and we only reclaim branches with no
// live checkout — and each run's branch/remote is run-namespaced, so a
// reclaim of one orphan can never touch a concurrent run's config.
// Best-effort: orphan-detection failures or partial removes correct
// themselves on the next bootstrap.
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
		// The marked branch IS the per-run local branch
		// (triagefactory/<runID>/pr-<n>); removePRConfigLocked reclaims its
		// config section, the branch ref + mirror, and the per-run push
		// remote it reconstructs from the branch name.
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
// The sweep uses this to decide whether a per-run PR branch is still
// actively backing a checkout — if its triagefactory/<runID>/pr-<n>
// branch is in this set, reclaiming its config would break that
// worktree, so the sweep leaves it for the run's own cleanup.
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

// configurePRPushTracking sets up the worktree's per-run bare-local branch so
// `git push` (no remote argument) lands on the real PR head — the contributor's
// fork branch for a fork PR, the upstream head for an own-repo PR. One uniform
// scheme for both: the only difference is pushURL (the fork's clone URL vs the
// upstream's, which the caller already resolved as headCloneURL). Configures:
//
//   - A remote tfpush-<runID>-<n> -> pushURL (in gitDir). Per (run, PR), not
//     per-PR: two concurrent runs on the same PR own separate remotes so one
//     run's cleanup can reclaim its remote without breaking the other.
//   - refs/remotes/<remoteName>/<headBranch>, pointed at localBranch's tip —
//     the exact remote-tracking ref the tfpush remote's default fetch refspec
//     (refs/heads/*:refs/remotes/<remote>/*) would produce, materialized
//     locally instead of via an actual fetch (which would need a separate
//     fork credential this path deliberately avoids). localBranch was just
//     fetched from refs/pull/<n>/head by the caller, so its tip IS the PR
//     head. Without this ref, branch.<localBranch>.merge below resolves to a
//     remote-tracking ref that was never created, and `git status` reports
//     "upstream is gone" on every PR run even though the push config is
//     intact.
//   - remote.<remoteName>.push as an explicit refspec mapping
//     refs/heads/<localBranch> -> refs/heads/<headBranch>. Both the fork and
//     own-repo paths now have local (triagefactory/<runID>/pr-<n>) differ from
//     the remote head branch, so under the default push.default ("simple")
//     `git push` (no args) would error "names don't match"; the explicit
//     refspec bypasses the check and pushes to the right place. Living on the
//     per-run remote (not a shared one) is what keeps two concurrent runs'
//     push refspecs from clobbering each other.
//   - branch.<localBranch>.remote / .merge so `git pull` treats the head repo
//     as the upstream and the agent can refresh.
//   - branch.<localBranch>.pushRemote so push targets the per-run remote even
//     if remote.pushDefault changes elsewhere.
//   - the trackedBranchMarkerKey (= the PR number) so SweepStaleForkPRConfig
//     can reclaim the block generically when inline cleanup doesn't fire.
//
// All keys are scoped to the run-namespaced branch / remote, so nothing here
// leaks across a concurrent run's worktree on the same shared bare. A
// deleted-fork PR (pushURL == "") never reaches this — its caller skips push
// tracking so `git push` fails loudly (read-only, by design).
//
// Idempotent: re-running on an already-configured state updates the URL and
// rewrites config keys to current values.
//
// gitDir is where the config is written: the shared bare for the linked-worktree
// path, or the run's own self-contained clone for the sandboxed multi-mode path
// (finishSelfContainedPRClone). The per-run namespacing of the remote/branch is
// load-bearing only for the shared bare — harmless but redundant in a per-run clone.
func configurePRPushTracking(ctx context.Context, gitDir, runID string, prNumber int, localBranch, pushURL, headBranch string) error {
	remoteName := prPushRemoteName(runID, prNumber)

	// Add or update the per-run push remote. `git remote add` errors when the
	// remote already exists; fall through to set-url in that case so repeat
	// calls (re-delegation, retries) are no-ops on URL match and corrective on
	// URL drift.
	if err := gitRunCtx(ctx, gitDir, "remote", "add", remoteName, pushURL); err != nil {
		if err := gitRunCtx(ctx, gitDir, "remote", "set-url", remoteName, pushURL); err != nil {
			return fmt.Errorf("add or set-url remote %s: %w", remoteName, err)
		}
	}

	// localBranch's tip in gitDir is the PR head the caller just fetched (via
	// refs/pull/<n>/head) — reuse it as the tfpush remote's tracking ref
	// instead of an actual fetch. See the doc comment above.
	headSHA, err := gitOutputCtx(ctx, gitDir, "rev-parse", "refs/heads/"+localBranch)
	if err != nil {
		return fmt.Errorf("resolve PR head sha for %s: %w", localBranch, err)
	}
	trackingRef := fmt.Sprintf("refs/remotes/%s/%s", remoteName, headBranch)
	if err := gitRunCtx(ctx, gitDir, "update-ref", trackingRef, strings.TrimSpace(headSHA)); err != nil {
		return fmt.Errorf("update-ref %s: %w", trackingRef, err)
	}

	pushRefspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", localBranch, headBranch)
	prMarker := strconv.Itoa(prNumber)
	cfgs := [][]string{
		{"config", fmt.Sprintf("remote.%s.push", remoteName), pushRefspec},
		{"config", fmt.Sprintf("branch.%s.remote", localBranch), remoteName},
		{"config", fmt.Sprintf("branch.%s.merge", localBranch), "refs/heads/" + headBranch},
		{"config", fmt.Sprintf("branch.%s.pushRemote", localBranch), remoteName},
		{"config", fmt.Sprintf("branch.%s.%s", localBranch, trackedBranchMarkerKey), prMarker},
	}
	for _, args := range cfgs {
		if err := gitRunCtx(ctx, gitDir, args...); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}
