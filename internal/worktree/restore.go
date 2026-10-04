package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// CheckoutRestore is one checkout a cold restore rebuilds under a run root:
// where it goes, what it tracks upstream, and the delta captured from it.
type CheckoutRestore struct {
	Owner, Repo string
	// CloneURL is the upstream the shared bare's origin points at. It seeds a
	// bare this host lacks and becomes a self-contained clone's origin. Empty
	// falls back to the origin a surviving bare already has.
	CloneURL string
	// Auth authenticates the rebuild's network git — seeding a missing bare,
	// refreshing what the checkout tracks, and the lazy promisor fetch a
	// checkout of the blobless bare triggers whether or not the bare was here.
	Auth CloneAuth
	// Root is the run root and Slug the checkout's conversation_worktrees ref
	// (pr-<N>, ref-<branch>, default). The checkout lands at
	// Root/Owner/Repo/Slug, exactly where `workspace add` would put it.
	Root, Slug string
	// RootKey namespaces the transient branch the rebuild stages in the shared
	// bare, and a pr-<N> checkout's push config when its captured branch is not
	// the run-namespaced PR branch.
	RootKey string
	// PR is the pull request a pr-<N> checkout is of, read fresh by the
	// caller. Required for a pr slug, ignored otherwise.
	PR *PRCheckout
	// Head and Branch are the captured HEAD and its branch ("" for detached).
	Head, Branch string
	// BundlePath and PatchPath are the captured members, staged on disk; ""
	// when the capture carried none.
	BundlePath, PatchPath string
}

// PRCheckout is what a pr-<N> checkout's push settings are re-derived from.
type PRCheckout struct {
	// HeadRef is the PR's head branch on its head repository.
	HeadRef string
	// HeadCloneURL is the head repository's URL in the bare origin's protocol,
	// "" when the head repository was deleted — the checkout is then
	// reviewable and read-only, as on a fresh run.
	HeadCloneURL string
	// BaseRef is the PR's base branch, refreshed for diff framing.
	BaseRef string
}

// RestoredCheckout is a checkout RestoreCheckout rebuilt, carrying what
// Discard needs to take it back out.
type RestoredCheckout struct {
	Owner, Repo string
	Path        string
	// PRNumber and PRKey name the push config a linked PR checkout wrote into
	// the shared bare; zero and "" when it wrote none there.
	PRNumber int
	PRKey    string
}

// ParseCheckoutSlug reverses CheckoutRefSlug and PRRefSlug: a "pr-<N>" slug
// yields prNumber N, "default" the empty ref, and "ref-<slug>" the branch with
// every '~' turned back into '/'. ok is false for anything those two never
// produce.
func ParseCheckoutSlug(slug string) (ref string, prNumber int, ok bool) {
	switch {
	case slug == "default":
		return "", 0, true
	case strings.HasPrefix(slug, "pr-"):
		n, err := strconv.Atoi(strings.TrimPrefix(slug, "pr-"))
		if err != nil || n <= 0 || PRRefSlug(n) != slug {
			return "", 0, false
		}
		return "", n, true
	case strings.HasPrefix(slug, "ref-"):
		ref = strings.ReplaceAll(strings.TrimPrefix(slug, "ref-"), "~", "/")
		if ValidateCheckoutRef(ref) != nil || CheckoutRefSlug(ref) != slug {
			return "", 0, false
		}
		return ref, 0, true
	}
	return "", 0, false
}

// commitSHAPattern is a full object id, SHA-1 or SHA-256.
var commitSHAPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// RestoreCheckout rebuilds one checkout the way `workspace add` builds it — a
// self-contained clone from the shared bare when the run is sandboxed, a linked
// worktree otherwise — positioned at the captured HEAD, with the captured
// uncommitted changes applied last.
//
// The order inside the per-repo lock:
//
//  1. Refresh what the checkout tracks upstream — the PR's head for a pr-<N>
//     checkout, the branch for ref-*/default, and the captured branch when it
//     was pushed — so the bundle's prerequisites are in the bare. A bare that
//     exists is not one that has them: a fresh executor's bare was cloned
//     without any refs/pull, and a surviving one may not have fetched since
//     the agent last pushed.
//  2. Import the bundle's objects. HEAD must then resolve in the bare.
//  3. Build the checkout. A self-contained clone takes a transient run-scoped
//     branch at HEAD — the createCheckoutCloneAt pattern — which is dropped
//     from the bare once the clone has copied the objects.
//  4. Re-derive push settings: a pr-<N> checkout gets the push tracking a
//     fresh --pr checkout gets, a ref-*/default one the origin fetch refspec a
//     fresh checkout leaves.
//
// Nothing of TF's is written into the checkout. On any failure the checkout and
// everything it put in the bare are removed, and the error is returned wrapped
// so a git host that could not be reached still reads as one
// (IsTransientGitError).
func RestoreCheckout(ctx context.Context, r CheckoutRestore) (RestoredCheckout, error) {
	ref, prNumber, ok := ParseCheckoutSlug(r.Slug)
	switch {
	case !ok:
		return RestoredCheckout{}, fmt.Errorf("restore checkout: unrecognized slug %q", r.Slug)
	case r.Owner == "" || r.Repo == "" || r.Root == "" || r.RootKey == "":
		return RestoredCheckout{}, fmt.Errorf("restore checkout %s: owner, repo, root and root key are required", r.Slug)
	case prNumber > 0 && r.PR == nil:
		return RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: pull request details required", r.Owner, r.Repo, r.Slug)
	case !commitSHAPattern.MatchString(r.Head):
		return RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: captured HEAD %q is not an object id", r.Owner, r.Repo, r.Slug, r.Head)
	}
	if r.Branch != "" {
		if err := validateBranchName(ctx, r.Branch); err != nil {
			return RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: %w", r.Owner, r.Repo, r.Slug, err)
		}
	}
	if r.PR != nil && prNumber > 0 && r.PR.BaseRef != "" {
		if err := ValidateCheckoutRef(r.PR.BaseRef); err != nil {
			return RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: base: %w", r.Owner, r.Repo, r.Slug, err)
		}
	}

	wtDir := filepath.Join(r.Root, r.Owner, r.Repo, r.Slug)
	if err := sandbox.MkdirRunTreeScaffold(r.Root, filepath.Join(r.Owner, r.Repo)); err != nil {
		return RestoredCheckout{}, fmt.Errorf("restore checkout: mkdir repo subdir: %w", err)
	}
	res := RestoredCheckout{Owner: r.Owner, Repo: r.Repo, Path: wtDir}

	if err := restoreCheckoutLocked(ctx, r, ref, prNumber, wtDir, &res); err != nil {
		res.Discard()
		return RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: %w", r.Owner, r.Repo, r.Slug, err)
	}
	if r.PatchPath != "" {
		if err := applyPatch(ctx, wtDir, r.PatchPath); err != nil {
			res.Discard()
			return RestoredCheckout{}, fmt.Errorf("restore checkout %s/%s %s: apply patch: %w", r.Owner, r.Repo, r.Slug, err)
		}
	}
	worktreeLog.Info("checkout restored", "dir", wtDir, "branch", r.Branch, "head", r.Head, "self_contained", selfContainedRunTrees())
	return res, nil
}

// Discard removes a restored checkout: its directory, its registration in the
// shared bare, and any push config it wrote there. Best-effort and idempotent —
// the all-or-nothing rollback of a multi-checkout restore calls it on every
// checkout that came back before one failed.
func (c RestoredCheckout) Discard() {
	if c.Path == "" {
		return
	}
	if err := sandbox.RemoveRunTree(context.Background(), c.Path); err != nil {
		worktreeLog.Warn("discard restored checkout: remove dir failed", "dir", c.Path, "error", err)
	}
	mu := lockRepo(c.Owner, c.Repo)
	mu.Lock()
	defer mu.Unlock()
	bareDir, err := repoDir(c.Owner, c.Repo)
	if err != nil {
		return
	}
	if _, err := os.Stat(bareDir); err != nil {
		return
	}
	removeWorktreeRegFor(bareDir, c.Path)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = gitRunCtx(ctx, bareDir, "worktree", "prune")
	if c.PRNumber > 0 && c.PRKey != "" {
		removePRConfigLocked(ctx, bareDir, prLocalBranch(c.PRKey, c.PRNumber), c.PRNumber)
	}
}

// restoreCheckoutLocked is RestoreCheckout's git work under the per-repo lock.
// It fills res as it writes into the bare, so a failure part-way leaves res
// naming exactly what Discard has to take back out.
func restoreCheckoutLocked(ctx context.Context, r CheckoutRestore, ref string, prNumber int, wtDir string, res *RestoredCheckout) error {
	mu := lockRepo(r.Owner, r.Repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, r.Owner, r.Repo, r.CloneURL, r.Auth)
	if err != nil {
		return fmt.Errorf("ensure bare: %w", err)
	}
	// The checkout's previous incarnation can still be registered — a linked
	// worktree whose directory went with the run root — and git refuses to add
	// over a registered path until it is pruned.
	if err := gitRunCtx(ctx, bareDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune worktrees: %w", err)
	}

	// 1. What the checkout tracks upstream.
	var (
		trackBranch string // the branch a clone's origin fetch refspec names
		prKey       string
		prLocal     string
		prHead      string
	)
	if prNumber > 0 {
		prKey = r.RootKey
		if k, n, ok := parsePRLocalBranch(r.Branch); ok && n == prNumber {
			prKey = k
		}
		prLocal = prLocalBranch(prKey, prNumber)
		mirror := "refs/remotes/origin/" + prLocal
		// Recorded before the fetch so a failure from here on reclaims the
		// mirror ref with the rest of the per-run PR state.
		res.PRNumber, res.PRKey = prNumber, prKey
		if err := gitRunCtxAuth(ctx, bareDir, r.Auth, "fetch", "origin", fmt.Sprintf("+refs/pull/%d/head:%s", prNumber, mirror)); err != nil {
			return fmt.Errorf("fetch PR #%d head: %w", prNumber, err)
		}
		out, err := gitOutputCtx(ctx, bareDir, "rev-parse", "--verify", mirror)
		if err != nil {
			return fmt.Errorf("resolve PR #%d head: %w", prNumber, err)
		}
		prHead = strings.TrimSpace(out)
		if base := r.PR.BaseRef; base != "" {
			trackBranch = base
			if err := gitRunCtxAuth(ctx, bareDir, r.Auth, "fetch", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base)); err != nil {
				worktreeLog.Warn("refresh PR base branch during restore failed; diff frames against recorded base / API instead", "number", prNumber, "base", base, "error", err)
			}
		}
	} else {
		if ref == "" {
			ref = detectDefaultBranch(ctx, bareDir)
		}
		trackBranch = ref
		if err := gitRunCtxAuth(ctx, bareDir, r.Auth, "fetch", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", ref, ref)); err != nil {
			// A branch deleted upstream since the checkout was made is not a
			// reason to lose the checkout: its commits may all be in the bundle
			// and the bare. An unreachable host is a reason to stop, and stays
			// readable as one.
			if IsTransientGitError(err) {
				return fmt.Errorf("fetch %s: %w", ref, err)
			}
			worktreeLog.Warn("refresh checkout branch during restore failed; restoring from what the bare and bundle hold", "ref", ref, "error", err)
		}
	}
	if r.Branch != "" && r.Branch != prLocal && r.Branch != trackBranch {
		// The agent's own branch, if it pushed it, holds the commits the
		// bundle stops at. Best-effort: a branch never pushed is not upstream.
		if err := gitRunCtxAuth(ctx, bareDir, r.Auth, "fetch", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", r.Branch, r.Branch)); err != nil {
			if IsTransientGitError(err) {
				return fmt.Errorf("fetch %s: %w", r.Branch, err)
			}
		}
	}

	// 2. The bundle's objects, then HEAD must resolve.
	if r.BundlePath != "" {
		rev := "HEAD"
		if r.Branch != "" {
			rev = "refs/heads/" + r.Branch
		}
		if err := gitRunCtx(ctx, bareDir, "fetch", r.BundlePath, rev); err != nil {
			return fmt.Errorf("import bundle: %w", err)
		}
	}
	if err := gitRunCtx(ctx, bareDir, "cat-file", "-e", r.Head+"^{commit}"); err != nil {
		return fmt.Errorf("captured HEAD %s is not in the bare after refreshing and importing the bundle: %w", r.Head, err)
	}

	// 3. The checkout.
	if selfContainedRunTrees() {
		return restoreSelfContainedLocked(ctx, r, bareDir, wtDir, trackBranch, prNumber, prKey, prLocal, prHead, res)
	}
	return restoreLinkedLocked(ctx, r, bareDir, wtDir, prNumber, prKey, prLocal, prHead)
}

// restoreSelfContainedLocked builds the checkout as a standalone clone of the
// bare at HEAD, staged through a transient run-scoped branch the way
// createCheckoutCloneAt stages a fresh one, then re-derives its origin and push
// settings inside the clone. Caller holds the per-repo lock.
func restoreSelfContainedLocked(ctx context.Context, r CheckoutRestore, bareDir, wtDir, trackBranch string, prNumber int, prKey, prLocal, prHead string, res *RestoredCheckout) error {
	// Push config lives in the clone, so the bare holds nothing a discard has
	// to reclaim once the per-run refs below are dropped.
	defer func() {
		if prLocal != "" {
			dropBareRunRefs(ctx, bareDir, prLocal)
		}
	}()
	res.PRKey = ""

	upstream := r.CloneURL
	if upstream == "" {
		out, err := gitOutputCtx(ctx, bareDir, "config", "--get", "remote.origin.url")
		if err != nil {
			return fmt.Errorf("read bare origin: %w", err)
		}
		upstream = strings.TrimSpace(out)
	}

	tmp := fmt.Sprintf("triagefactory/%s/restore", r.RootKey)
	if err := gitRunCtx(ctx, bareDir, "branch", "-f", tmp, r.Head); err != nil {
		return fmt.Errorf("stage restore branch: %w", err)
	}
	defer dropBareRunRefs(ctx, bareDir, tmp)
	if err := materializeSelfContainedClone(ctx, bareDir, wtDir, tmp, trackBranch, upstream, r.Auth); err != nil {
		return err
	}

	// Leave the clone on the captured branch, or detached, with every trace of
	// the transient branch gone — the ref, its config section, and the
	// clone-time remote-tracking mirror.
	if r.Branch != "" {
		if err := gitRunCtx(ctx, wtDir, "branch", "-M", tmp, r.Branch); err != nil {
			return fmt.Errorf("name restored branch %s: %w", r.Branch, err)
		}
		_ = gitRunCtx(ctx, wtDir, "config", "--remove-section", "branch."+r.Branch)
	} else {
		if err := gitRunCtx(ctx, wtDir, "checkout", "--detach"); err != nil {
			return fmt.Errorf("detach restored clone: %w", err)
		}
		if err := gitRunCtx(ctx, wtDir, "branch", "-D", tmp); err != nil {
			return fmt.Errorf("drop staging branch from restored clone: %w", err)
		}
		_ = gitRunCtx(ctx, wtDir, "config", "--remove-section", "branch."+tmp)
	}
	_ = gitRunCtx(ctx, wtDir, "update-ref", "-d", "refs/remotes/origin/"+tmp)
	if trackBranch != "" {
		if err := gitRunCtx(ctx, wtDir, "config", "remote.origin.fetch",
			fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", trackBranch, trackBranch)); err != nil {
			return fmt.Errorf("repoint clone fetch refspec: %w", err)
		}
	}

	if prNumber == 0 {
		return nil
	}
	// The PR head is on a remote-tracking ref, as in a fresh clone of it: the
	// next snapshot's bundle stops at what GitHub already has.
	if err := gitRunCtx(ctx, wtDir, "update-ref", "refs/remotes/origin/"+prLocal, prHead); err != nil {
		return fmt.Errorf("record PR head in restored clone: %w", err)
	}
	return restorePRPushLocked(ctx, r, wtDir, prNumber, prKey, prLocal, prHead)
}

// restoreLinkedLocked builds the checkout as a linked worktree of the shared
// bare at HEAD — on the captured branch, forced to HEAD, or detached — and
// re-derives a PR checkout's push config in the bare. Caller holds the
// per-repo lock and has left the PR head on the bare's mirror ref.
func restoreLinkedLocked(ctx context.Context, r CheckoutRestore, bareDir, wtDir string, prNumber int, prKey, prLocal, prHead string) error {
	if r.Branch != "" {
		if err := gitRunCtx(ctx, bareDir, "branch", "-f", r.Branch, r.Head); err != nil {
			return fmt.Errorf("position branch %s: %w", r.Branch, err)
		}
		if err := gitRunCtxAuth(ctx, bareDir, r.Auth, "worktree", "add", wtDir, r.Branch); err != nil {
			removeWorktreeRegFor(bareDir, wtDir)
			return fmt.Errorf("worktree add: %w", err)
		}
	} else if err := gitRunCtxAuth(ctx, bareDir, r.Auth, "worktree", "add", "--detach", wtDir, r.Head); err != nil {
		removeWorktreeRegFor(bareDir, wtDir)
		return fmt.Errorf("worktree add --detach: %w", err)
	}
	if prNumber == 0 {
		return nil
	}
	return restorePRPushLocked(ctx, r, bareDir, prNumber, prKey, prLocal, prHead)
}

// restorePRPushLocked gives a restored PR checkout the push tracking a fresh
// --pr checkout gets, in gitDir — the clone, or the shared bare. The
// run-namespaced PR branch is recreated at the PR head when the agent had moved
// off it, so `git push` from it lands on the PR exactly as it would have. A
// deleted head repository leaves the checkout read-only, as on a fresh run.
func restorePRPushLocked(ctx context.Context, r CheckoutRestore, gitDir string, prNumber int, prKey, prLocal, prHead string) error {
	if !branchExists(gitDir, prLocal) {
		if err := gitRunCtx(ctx, gitDir, "branch", prLocal, prHead); err != nil {
			return fmt.Errorf("recreate PR branch %s: %w", prLocal, err)
		}
	}
	if r.PR.HeadCloneURL == "" {
		worktreeLog.Warn("PR head repository unavailable (deleted fork); restored checkout is read-only", "number", prNumber)
		return nil
	}
	if err := configurePRPushTrackingAt(ctx, gitDir, prKey, prNumber, prLocal, r.PR.HeadCloneURL, r.PR.HeadRef, prHead); err != nil {
		return fmt.Errorf("configure PR push tracking: %w", err)
	}
	return nil
}

// validateBranchName holds a captured branch name to git's own rule for one
// before it is spelled into a refspec. The name is read off a checkout an agent
// could write to, so it is not trusted to be one git produced.
func validateBranchName(ctx context.Context, branch string) error {
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	if _, err := gitOutputCtx(ctx, os.TempDir(), "check-ref-format", "refs/heads/"+branch); err != nil {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	return nil
}

// PRBranchKey returns the key a run-namespaced PR branch
// (triagefactory/<key>/pr-<N>) is namespaced by, when branch is one for
// prNumber. It names the per-run push config a linked checkout on that branch
// left in the shared bare, which CleanupPRConfig reclaims by the same key.
func PRBranchKey(branch string, prNumber int) (string, bool) {
	key, n, ok := parsePRLocalBranch(branch)
	if !ok || n != prNumber {
		return "", false
	}
	return key, true
}
