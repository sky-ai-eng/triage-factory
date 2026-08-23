package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// CreateForBranch sets up a worktree on a new feature branch based off
// a given base, at the run's default location runDir(rootKey). If
// baseBranch is empty, the repo's default branch is detected from
// origin/HEAD. Used by the eager GitHub PR delegation path where the
// run has exactly one repo.
func CreateForBranch(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, rootKey string, opts ...CloneOption) (string, error) {
	wtDir, err := makeWorktreeDir(rootKey)
	if err != nil {
		return "", err
	}
	return createBranchWorktreeAt(ctx, owner, repo, cloneURL, baseBranch, featureBranch, rootKey, wtDir, resolveCloneOptions(opts).auth)
}

// CreateForBranchInRoot is the lazy-Jira-delegation variant: the worktree
// lands at filepath.Join(runRoot, owner, repo) so a single run can host
// multiple per-repo worktrees as siblings under a shared run-root. The
// run-root must already exist (created by MakeRunRoot in the spawner);
// the owner-level subdir is created here.
//
// Other than the path, behavior matches CreateForBranch — same per-repo
// lock, same bare-clone reuse, same branch-exists reattach, same excludes
// rollback.
//
// No live production caller: `workspace add` routes to CreateForCheckoutInRoot
// (the detached default/--ref path, TFAC-498), not here. Retained as the
// prescribed-feature-branch variant for a future caller that needs a named
// branch checked out up front rather than a detached checkout the agent
// branches from itself.
func CreateForBranchInRoot(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, rootKey, runRoot string) (string, error) {
	if runRoot == "" {
		return "", fmt.Errorf("CreateForBranchInRoot: runRoot is required")
	}
	wtDir := filepath.Join(runRoot, owner, repo)
	if err := sandbox.MkdirRunTreeScaffold(runRoot, owner); err != nil {
		return "", fmt.Errorf("mkdir owner subdir: %w", err)
	}
	// No CloneAuth here: this is the in-sandbox Jira `workspace add` path
	// (cmd/exec/workspace), where in-sandbox git credentials are a separate
	// concern, not the host-side clone path. The shared body is already
	// auth-capable, so a credential can be threaded through when it wires
	// the in-sandbox path.
	return createBranchWorktreeAt(ctx, owner, repo, cloneURL, baseBranch, featureBranch, rootKey, wtDir, CloneAuth{})
}

// CheckoutRefSlug is the conversation_worktrees ref (PK discriminator) AND the
// worktree-subdir name for a default/--ref checkout. It MUST be a single
// filesystem-safe path segment that is injective over valid refs (distinct refs
// → distinct slugs) and disjoint from the reserved "pr-<N>" and "default"
// forms. Otherwise one run would treat two different `workspace add --ref`
// targets — or a branch literally named "pr-42" and PR #42 — as the same
// worktree: the idempotent re-add hands back the wrong checkout, and the
// detached worktree the agent branches/pushes from is the wrong one.
//
//   - ""         → "default"       (the selector "no ref named → the repo's
//     default branch"; a plain word, chosen NOT to look like a git revision —
//     the slug lands in every agent-visible path, and a ref-shaped name there
//     reads as "the agent is working on the default branch", which a detached
//     checkout precisely is not)
//   - "<branch>" → "ref-<branch>"  with every '/' replaced by '~'
//
// Two properties make this collision-free:
//
//   - Every branch slug carries the "ref-" prefix and every PR slug the "pr-"
//     prefix, so the bare word "default" collides with neither — a branch
//     literally named "default" slugs to "ref-default".
//   - A validated --ref's alphabet is [A-Za-z0-9._/-] (see validateGitRef), so
//     '/' is the only path-unsafe byte AND '~' can never appear literally in the
//     input. Replacing only '/' → '~' is therefore injective: "feature/foo" →
//     "ref-feature~foo" and "feature-foo" → "ref-feature-foo" stay distinct
//     (the old fold-everything-to-'-' scheme collapsed both to "feature-foo").
//
// The slug is never handed to git (the fetch uses the raw ref); it is only the
// PK + on-disk subdir + `workspace list` display. Exported so the workspace CLI
// reserves the conversation_worktrees row and computes the worktree path with the same
// slug CreateForCheckoutInRoot lands the worktree at.
func CheckoutRefSlug(ref string) string {
	if ref == "" {
		return "default"
	}
	return "ref-" + strings.ReplaceAll(ref, "/", "~")
}

// CreateForCheckoutInRoot materializes a worktree at filepath.Join(runRoot,
// owner, repo, ref-slug) checked out — in DETACHED HEAD — at the fresh tip of
// an existing branch on origin. When ref is empty the repo's default branch is
// detected and used (slug "default"). This is the generalized `workspace add`
// / `workspace add --ref <branch>` path (TFAC-498): unlike CreateForBranchInRoot
// it does NOT mint a prescribed feature branch — it hands back a checkout of the
// named branch as-is and lets the agent create its own working branch (`git
// checkout -b ...`) before pushing. The push gate then authorizes whatever
// branch the worktree lands on (its live current branch), not a prescribed name.
//
// Detached (rather than a local branch) is deliberate: many runs default to the
// same repo's default branch, and git refuses to check out one local branch ref
// in two worktrees of a shared bare. A detached checkout claims no branch ref,
// so concurrent runs sharing this bare never collide; a detached HEAD also
// yields no live branch, so the push gate authorizes nothing until the agent
// has created its own branch — exactly the intended flow.
//
// The ref-slug subdir lets one run hold several checkouts in one repo (TFAC-502).
// The run-root must already exist (created by MakeRunRoot in the spawner); the
// owner/repo subdirs are created here.
//
// Since TFAC-546 this runs HOST-SIDE in both modes (the agenthost daemon calls
// it on the sandbox's behalf in multi), so WithCloneAuth is honored for the
// clone/fetch. Local mode routes to the zero-copy linked worktree as before;
// a sandboxed run materializes a SELF-CONTAINED clone — the run root is the
// only tree inside the sandbox, so a worktree's .git pointer into the shared
// bare would dangle there (mirrors CreateForPR's split). Lazy materialization
// takes the same branch as the eager one, so a repo added mid-run needs no new
// mount.
func CreateForCheckoutInRoot(ctx context.Context, owner, repo, cloneURL, ref, rootKey, runRoot string, opts ...CloneOption) (string, error) {
	if runRoot == "" {
		return "", fmt.Errorf("CreateForCheckoutInRoot: runRoot is required")
	}
	// Guard at the interpolation point: ref lands in a fetch refspec below, and
	// callers now include the agenthost RPC surface, which must not rely on the
	// workspace CLI's argv validation having run in front of it.
	if ref != "" {
		if err := ValidateCheckoutRef(ref); err != nil {
			return "", err
		}
	}
	wtDir := filepath.Join(runRoot, owner, repo, CheckoutRefSlug(ref))
	if err := sandbox.MkdirRunTreeScaffold(runRoot, filepath.Join(owner, repo)); err != nil {
		return "", fmt.Errorf("mkdir repo subdir: %w", err)
	}
	auth := resolveCloneOptions(opts).auth
	if selfContainedRunTrees() {
		return createCheckoutCloneAt(ctx, owner, repo, cloneURL, ref, rootKey, wtDir, auth)
	}
	return createCheckoutWorktreeAt(ctx, owner, repo, cloneURL, ref, rootKey, wtDir, auth)
}

// checkoutRefPattern restricts a checkout ref to a conservative refname
// alphabet before it's interpolated into a fetch refspec and passed to git.
// Uppercase is permitted (branch names routinely carry ticket keys like
// PROJ-220).
//
// Blocks: leading dash (interpreted as a git CLI flag), whitespace, shell
// metacharacters (`;`, `|`, backticks, `$`), refname-illegal characters
// (`:`, `?`, `*`, `[`, `~`, `^`, `\`, control bytes). The `..` substring is
// rejected separately — it's lexically allowed by the char class but git
// refnames forbid it (and it enables path traversal in the worktree dir).
var checkoutRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,128}$`)

// ValidateCheckoutRef rejects a branch ref that a checkout entry point would
// otherwise interpolate into a fetch refspec: anything outside the conservative
// alphabet above, plus the shapes the alphabet admits but git-check-ref-format
// still refuses — the `..` substring, a trailing slash or dot, an empty
// component (consecutive slashes), a component starting with "." or ending in
// ".lock" — and a "refs/" prefix (we'd double it up into
// "+refs/heads/refs/heads/..." → an opaque "couldn't find remote ref").
//
// Lives here — rather than only in the workspace CLI's argv parsing — because
// the checkout creates are now reachable from the agenthost RPC surface too
// (TFAC-546), and the guard belongs at the interpolation point. Exported so
// both front doors (`workspace add --ref` and the RPC) share one rule.
func ValidateCheckoutRef(ref string) error {
	if !checkoutRefPattern.MatchString(ref) ||
		strings.Contains(ref, "..") ||
		strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") ||
		strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("invalid git ref %q", ref)
	}
	for _, comp := range strings.Split(ref, "/") {
		if comp == "" || strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return fmt.Errorf("invalid git ref %q", ref)
		}
	}
	return nil
}

// createCheckoutWorktreeAt fetches ref fresh from origin and adds a detached
// worktree at its tip. Empty ref → the repo's default branch. Shares the
// per-repo lock, bare-clone reuse, and exclude-or-rollback with the other
// Create* helpers; differs in that it never creates or reattaches a local
// branch — the checkout is detached.
func createCheckoutWorktreeAt(ctx context.Context, owner, repo, cloneURL, ref, rootKey, wtDir string, auth CloneAuth) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, cloneURL, auth)
	if err != nil {
		return "", err
	}

	if ref == "" {
		ref = detectDefaultBranch(ctx, bareDir)
	}

	// Fetch the ref fresh into its remote-tracking ref (never the local branch
	// ref — same conflict-avoidance reasoning as createBranchWorktreeAt: a
	// local branch ref may be live in another worktree of this shared bare).
	remoteRef := "refs/remotes/origin/" + ref
	fetchSpec := fmt.Sprintf("+refs/heads/%s:%s", ref, remoteRef)
	start := time.Now()
	if err := gitRunCtxAuth(ctx, bareDir, auth, "fetch", "origin", fetchSpec); err != nil {
		return "", fmt.Errorf("fetch ref %s: %w", ref, err)
	}
	worktreeLog.Debug("fetch completed", "ref", ref, "duration", time.Since(start).Round(time.Millisecond))

	// Detached checkout at the fetched tip. Routed through gitRunCtxAuth so the
	// blobless bare's lazy promisor fetch (working-tree blobs deferred by the
	// partial clone) authenticates against origin on a private repo.
	if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", "--detach", wtDir, remoteRef); err != nil {
		// A cancelled/killed add can leave wtDir half-built and the bare's
		// worktree registration behind. Reclaim only THIS add (keyed on wtDir)
		// so a concurrent add against the same bare isn't disturbed.
		_ = os.RemoveAll(wtDir)
		removeWorktreeRegFor(bareDir, wtDir)
		return "", fmt.Errorf("worktree add (detached %s): %w", ref, err)
	}

	if err := addExcludesOrRollback(rootKey, wtDir); err != nil {
		return "", err
	}

	worktreeLog.Debug("checkout worktree at", "dir", wtDir, "ref", ref, "detached", true)
	return wtDir, nil
}

// createCheckoutCloneAt is the multi-mode counterpart of
// createCheckoutWorktreeAt: same fetch-fresh-from-origin semantics, but the
// checkout is materialized as a SELF-CONTAINED clone (its own .git directory)
// instead of a linked worktree, because the run root is bind-mounted into a
// sandbox that can't see the shared bare (TFAC-546, mirroring TFAC-545's
// finishSelfContainedPRClone). The bare stays the object source — one remote
// fetch, reused across runs — and only this per-run copy is new.
//
// The clone ends DETACHED at the fetched tip, preserving the checkout-path
// invariant: no live branch → the push gate authorizes nothing until the agent
// creates its own branch. git can only clone a local branch, so a transient
// run-namespaced branch (triagefactory/<rootKey>/checkout) carries the tip
// through the clone and is deleted from both the clone and the bare afterwards.
func createCheckoutCloneAt(ctx context.Context, owner, repo, cloneURL, ref, rootKey, wtDir string, auth CloneAuth) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, cloneURL, auth)
	if err != nil {
		return "", err
	}

	if ref == "" {
		ref = detectDefaultBranch(ctx, bareDir)
	}

	// Fetch the ref fresh into its remote-tracking ref — identical to the
	// worktree path, and materializeSelfContainedClone copies this ref into the
	// clone (its baseBranch parameter) so the agent has an origin/<ref> to diff
	// and branch against.
	remoteRef := "refs/remotes/origin/" + ref
	fetchSpec := fmt.Sprintf("+refs/heads/%s:%s", ref, remoteRef)
	start := time.Now()
	if err := gitRunCtxAuth(ctx, bareDir, auth, "fetch", "origin", fetchSpec); err != nil {
		return "", fmt.Errorf("fetch ref %s: %w", ref, err)
	}
	worktreeLog.Debug("fetch completed", "ref", ref, "duration", time.Since(start).Round(time.Millisecond))

	// Transient bare-local branch at the fetched tip, run-namespaced so
	// concurrent runs sharing this bare never collide on it. -f overwrites a
	// stray leftover from a crashed prior create (nothing checks it out — the
	// clone below copies it and both copies are deleted before the lock drops).
	tmpBranch := fmt.Sprintf("triagefactory/%s/checkout", rootKey)
	if err := gitRunCtx(ctx, bareDir, "branch", "-f", tmpBranch, remoteRef); err != nil {
		return "", fmt.Errorf("stage checkout branch %s: %w", tmpBranch, err)
	}

	if err := materializeSelfContainedClone(ctx, bareDir, wtDir, tmpBranch, ref, cloneURL, auth); err != nil {
		_ = os.RemoveAll(wtDir)
		dropBareRunRefs(ctx, bareDir, tmpBranch)
		return "", err
	}
	dropBareRunRefs(ctx, bareDir, tmpBranch)

	// Detach at the tip, then remove every trace of the transient branch from
	// the clone: the branch ref itself, its config section, the clone-time
	// remote-tracking mirror, and the --single-branch fetch refspec (repointed
	// at the real ref so an in-sandbox `git fetch` refreshes origin/<ref>
	// instead of failing on a branch that never existed upstream).
	if err := gitRunCtx(ctx, wtDir, "checkout", "--detach"); err != nil {
		_ = os.RemoveAll(wtDir)
		return "", fmt.Errorf("detach run clone: %w", err)
	}
	if err := gitRunCtx(ctx, wtDir, "branch", "-D", tmpBranch); err != nil {
		_ = os.RemoveAll(wtDir)
		return "", fmt.Errorf("drop staging branch from run clone: %w", err)
	}
	_ = gitRunCtx(ctx, wtDir, "config", "--remove-section", "branch."+tmpBranch)
	_ = gitRunCtx(ctx, wtDir, "update-ref", "-d", "refs/remotes/origin/"+tmpBranch)
	if err := gitRunCtx(ctx, wtDir, "config", "remote.origin.fetch",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", ref, ref)); err != nil {
		_ = os.RemoveAll(wtDir)
		return "", fmt.Errorf("repoint clone fetch refspec: %w", err)
	}

	if err := writeLocalExcludes(wtDir); err != nil {
		_ = os.RemoveAll(wtDir)
		return "", fmt.Errorf("write local git excludes: %w", err)
	}

	plantSandboxSkillsLink(wtDir)

	worktreeLog.Info("checkout run clone created (self-contained)", "dir", wtDir, "ref", ref, "detached", true)
	return wtDir, nil
}

// CurrentBranch returns the worktree's checked-out branch (the short name HEAD
// symbolically points at), or "" when HEAD is detached, the path isn't a git
// worktree, or HEAD can't be read. The push gate (internal/delegate) uses this
// to authorize "whatever branch the checkout is currently on" rather than a
// branch prescribed when the worktree was reserved: a detached HEAD — the state a fresh
// default / --ref `workspace add` lands in — yields "" so no push is authorized
// until the agent creates its own branch.
//
// It reads .git/HEAD as a plain file rather than shelling out to git. In multi
// mode the run root is chowned to the sandbox uid, so its .git/config is
// agent-writable; ANY git invocation there (even `symbolic-ref`) bootstraps the
// repository config on startup and follows an attacker-planted include.path /
// includeIf, opening+parsing whatever file it names as this (host-root)
// process. A byte read of HEAD enters no repository, runs no git, and consults
// no config, so there is nothing for a hostile .git/config to steer.
func CurrentBranch(path string) string {
	gitDir, _, ok := worktreeGitPaths(path)
	if !ok {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	const p = "ref: refs/heads/"
	ref := strings.TrimSpace(string(b))
	if !strings.HasPrefix(ref, p) {
		return "" // detached HEAD (raw SHA) or a non-branch symref: no branch
	}
	return strings.TrimPrefix(ref, p)
}

// worktreeGitPaths resolves a worktree root's per-worktree git dir and its
// common git dir WITHOUT invoking git, so it never enters a repository and thus
// never triggers git's startup config bootstrap (which would open+parse an
// agent-writable include.path as this process). A plain clone's ".git" is a
// directory that is both; a linked worktree's ".git" is a "gitdir: <path>"
// pointer whose shared config lives in the common dir. ok is false when root is
// not a worktree.
func worktreeGitPaths(root string) (gitDir, commonDir string, ok bool) {
	if root == "" {
		return "", "", false
	}
	dot := filepath.Join(root, ".git")
	fi, err := os.Lstat(dot)
	if err != nil {
		return "", "", false
	}
	if fi.IsDir() {
		return dot, dot, true // plain clone: per-worktree dir == common dir
	}
	b, err := os.ReadFile(dot)
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(string(b))
	p := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if p == "" || p == line {
		return "", "", false // not a "gitdir:" pointer
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	gitDir = filepath.Clean(p)
	commonDir = gitDir
	// A linked worktree records the shared common dir (holding the shared
	// config) in <gitDir>/commondir, as an absolute or gitDir-relative path.
	if cb, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		if c := strings.TrimSpace(string(cb)); c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(gitDir, c)
			}
			commonDir = filepath.Clean(c)
		}
	}
	return gitDir, commonDir, true
}

// PushTargetBranch returns the REMOTE branch a bare `git push` from the
// worktree's current checkout would update, or "" when HEAD is detached (no
// push possible) or the configured push refspec targets a non-branch namespace
// (refs/for/..., refs/tags/... — nothing the branch gate can authorize).
//
// It resolves the mapping exactly the way git does for an argument-less push:
// the push remote is branch.<b>.pushRemote, then remote.pushDefault, then
// branch.<b>.remote, then "origin"; if that remote carries an explicit
// remote.<r>.push refspec whose source is the current branch, the refspec's
// destination is the target. With no matching refspec the target is the branch
// itself (push.default "simple" — same name upstream).
//
// This is what the push-authorization gate (internal/delegate) must compare
// receive-pack commands against: the command block carries the REMOTE ref, and
// for a PR worktree that differs from the local checkout by construction — the
// checkout is the run-namespaced triagefactory/<rootKey>/pr-<n> while
// configurePRPushTracking maps it to the PR's real head branch. Authorizing the
// local name (the old CurrentBranch rule) both broke every PR push (the pushed
// head ref was never in the allowlist) and authorized a stray run-namespaced
// branch upstream instead.
//
// The config this reads lives in the run root, which a sandboxed agent can
// edit — but that grants no authority the live-branch rule didn't already
// concede (the agent can equally `git checkout -b <any-name>`); the caller
// still refuses base/protected branches on the RESOLVED target, so the
// protected floor holds regardless of what the config maps to.
func PushTargetBranch(path string) string {
	branch := CurrentBranch(path)
	if branch == "" {
		return ""
	}
	remote := gitConfigFirst(path,
		"branch."+branch+".pushRemote",
		"remote.pushDefault",
		"branch."+branch+".remote",
	)
	if remote == "" {
		remote = "origin"
	}
	for _, spec := range gitConfigAll(path, "remote."+remote+".push") {
		src, dst, ok := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if !ok {
			continue
		}
		if src != branch && src != "refs/heads/"+branch {
			continue
		}
		if target, ok := strings.CutPrefix(dst, "refs/heads/"); ok {
			return target
		}
		if strings.HasPrefix(dst, "refs/") {
			return "" // non-branch destination namespace: nothing to authorize
		}
		return dst
	}
	return branch
}

// gitConfigFirst returns the first non-empty single-valued git config entry
// among keys, read from the worktree at path ("" when none is set).
func gitConfigFirst(path string, keys ...string) string {
	_, commonDir, ok := worktreeGitPaths(path)
	if !ok {
		return ""
	}
	cfg := filepath.Join(commonDir, "config")
	for _, key := range keys {
		out, err := gitConfigFileValue(cfg, "--get", key)
		if err != nil {
			continue // unset key exits non-zero — a normal absent state
		}
		if v := strings.TrimSpace(out); v != "" {
			return v
		}
	}
	return ""
}

// gitConfigAll returns every value of a multi-valued git config key, read from
// the worktree at path (nil when unset).
func gitConfigAll(path, key string) []string {
	_, commonDir, ok := worktreeGitPaths(path)
	if !ok {
		return nil
	}
	out, err := gitConfigFileValue(filepath.Join(commonDir, "config"), "--get-all", key)
	if err != nil {
		return nil
	}
	var vals []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			vals = append(vals, line)
		}
	}
	return vals
}

// gitConfigFileValue reads config values from a single file with `git config
// --file <configPath> --no-includes`, executed from a neutral non-repository
// working directory. That combination is what keeps an agent-writable
// .git/config (the sandbox-chowned run root, read here host-side as root in
// multi mode) from steering this read into opening an attacker-chosen file:
//
//   - Running OUTSIDE any repository means git performs no startup repository
//     config bootstrap, and it is that bootstrap — not the --file read — that
//     follows the config's own include.path / includeIf. (Inside the repo, even
//     `git config --file X` still bootstraps the ambient repo config and opens
//     its includes.)
//   - --no-includes then suppresses include directives within configPath
//     itself. On its own --no-includes is NOT sufficient: git honors it for a
//     single-key `--get` but ignores it for `--get-all` / `--get-regexp` /
//     `--list`, and there is no equivalent for a non-config command like
//     `symbolic-ref` at all — which is why the branch read reads HEAD directly
//     and every config read runs from outside the repo.
//
// Because no repository is discovered, git runs no dubious-ownership check, so
// reading a config file the sandbox uid owns needs no safe.directory grant.
func gitConfigFileValue(configPath string, getArgs ...string) (string, error) {
	args := append([]string{"config", "--file", configPath, "--no-includes"}, getArgs...)
	cmd := exec.CommandContext(context.Background(), "git", args...)
	// A neutral CWD with no ancestor .git so git discovers no repository (and
	// GIT_DIR/GIT_WORK_TREE dropped so an inherited value can't reintroduce one).
	cmd.Dir = os.TempDir()
	cmd.Env = gitConfigReadEnv()
	return runGitOutput(context.Background(), cmd, args)
}

// gitConfigReadEnv is gitBaseEnv with GIT_DIR / GIT_WORK_TREE stripped, so a
// value inherited from the parent process can't pull gitConfigFileValue back
// into a repository (and thus back into the startup include-following it exists
// to avoid).
func gitConfigReadEnv() []string {
	base := gitBaseEnv()
	out := base[:0:0]
	for _, kv := range base {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// createBranchWorktreeAt is the shared body of the two CreateForBranch
// variants — bare-clone setup, base-branch fetch, `git worktree add`
// (with branchExists reattach), and exclude-or-rollback. The two
// public callers differ only in where wtDir lives on disk.
func createBranchWorktreeAt(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, rootKey, wtDir string, auth CloneAuth) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, cloneURL, auth)
	if err != nil {
		return "", err
	}

	// Fetch the base branch into the remote-tracking ref rather than
	// the local branch ref. If a caller keeps that base branch checked out
	// as a real local branch elsewhere in this bare's worktrees, fetching
	// with `+refs/heads/<b>:refs/heads/<b>` would have git refuse with
	// "fatal: refusing to fetch into branch '<b>' checked out at '<path>'".
	// Fetching into refs/remotes/origin/<b> sidesteps the conflict. The new
	// feature branch is then created off the just-fetched remote-tracking
	// ref.
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

	if err := addExcludesOrRollback(rootKey, wtDir); err != nil {
		return "", err
	}
	plantSandboxSkillsLink(wtDir)

	worktreeLog.Debug("branch worktree at", "dir", wtDir, "branch", featureBranch, "base", baseBranch)
	return wtDir, nil
}

// addExcludesOrRollback wraps writeLocalExcludes with the rollback both
// Create* functions need: if the exclude write fails, the worktree is
// already registered with the bare repo and on disk, so we must remove
// it before returning. Without rollback the caller sees an error but
// has no handle to clean up with, leaking a half-configured worktree
// and its bare-repo registration.
func addExcludesOrRollback(rootKey, wtDir string) error {
	if err := writeLocalExcludes(wtDir); err != nil {
		if rmErr := RemoveAt(wtDir, rootKey); rmErr != nil {
			worktreeLog.Warn("rollback after exclude-write failure", "error", rmErr)
		}
		return fmt.Errorf("write local git excludes: %w", err)
	}
	return nil
}

// ScratchDir is the one directory TF claims inside a run tree: CI log archives,
// ephemeral downloads, the agent's own memory.md, the entity-memory subdir
// the spawner populates, and whatever intermediates the agent writes. Every
// producer of a path under it — spawner, exec verbs, prompts — names it
// through this constant.
//
// The name is deliberately ours rather than descriptive. For a GitHub PR run
// the run tree IS the repo checkout, so this directory lands in someone else's
// source tree: a plausible generic name is a name a repo might already use, and
// a collision there means TF writing over, or deleting, tracked content that
// then rides the agent's next commit.
const ScratchDir = "_tfac"

// CILogsDir is the subdirectory of ScratchDir that `exec gh actions
// download-logs` extracts a workflow run's log archive into, one <run_id>
// subtree per run. It is named here rather than at either use site because two
// packages have to agree on it: the exec verb writes that tree, and the
// workspace snapshot skips it as re-downloadable rather than carrying a log
// archive in the blob. A rename that reached only one of them would silently
// put GBs of logs back in every snapshot.
const CILogsDir = "ci-logs"

// legacyScratchDir is what ScratchDir was called before. It survives in two
// places on purpose: the managed exclude list, so a tree built by an older
// binary can't leak its leftovers into a commit, and AdoptLegacyScratchDir.
const legacyScratchDir = "_scratch"

// managedExcludePatterns are the gitignore patterns writeLocalExcludes
// ensures are present in .git/info/exclude for every delegated worktree.
// One prefix covers everything under it.
var managedExcludePatterns = []string{ScratchDir + "/", legacyScratchDir + "/"}

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
