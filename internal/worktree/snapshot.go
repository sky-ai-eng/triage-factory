// Snapshot/rehydrate git primitives for the durable blueprint workspace.
// CaptureWorkspaceGit distills a parked worktree down to its non-recoverable
// git state (the agent's local-only commits + the uncommitted working tree);
// RestoreWorkspaceGit rebuilds a worktree from the persistent bare clone and
// layers that delta back on. The split keeps git knowledge in this package
// while the storage/tar orchestration lives in internal/delegate.

package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// GitDelta is the non-recoverable git state of a delegated worktree at a
// dormancy point. Everything recoverable — the bare clone, anything already
// pushed to a remote — is deliberately absent; RestoreWorkspaceGit
// reconstructs those from the bare and layers this delta on top. A nil
// *GitDelta means the path was not a git worktree (e.g. a Jira lazy run-root),
// so there is no git state to carry.
//
// SECURITY INVARIANT — the agent's .git/config is NEVER part of this delta,
// and must never be added. Restore rebuilds the worktree from the host-owned
// bare, so the config in effect during the restore-side `git apply` is the
// bare's clean config, not anything the agent authored. That is the only
// reason restore's apply is safe against a hostile checkout: a tracked
// .gitattributes can select a filter driver, but only config defines the
// command that driver runs, and restore's config is ours. Carrying the
// agent's config across a snapshot (or applying it into the rebuilt worktree)
// would arm the restore-side apply the same way an agent-authored config arms
// the capture path — keep config out of the delta.
type GitDelta struct {
	// Branch is the worktree's checked-out branch. Empty when HEAD is
	// detached — restore then checks out Head directly (detached).
	Branch string
	// Head is the HEAD commit SHA, always captured. It's the authoritative
	// tip restore positions the worktree at, and it's what lets restore
	// recreate the branch even when the bundle is empty and the branch was
	// never pushed to a remote (the never-pushed-local-branch case), or check
	// out a detached HEAD (no branch at all).
	Head string
	// Bundle is a `git bundle` of the commits reachable from Head but not
	// from any remote-tracking ref — i.e. only the agent's local-only work.
	// nil when the tip is already on a remote (nothing local to carry); the
	// bare reproduces the committed state by itself in that case, and Head is
	// reachable there.
	Bundle []byte
	// Patch is a single binary diff capturing every uncommitted change
	// (tracked modifications and untracked additions alike), with the managed
	// _scratch tree excluded. nil when the working tree is clean.
	Patch []byte
}

// IsGitWorktree reports whether wtPath is the root of a git working tree (a
// `.git` file for a linked worktree, or a `.git` directory for a plain
// checkout). False for an empty path or a Jira lazy run-root that holds only
// _scratch.
func IsGitWorktree(wtPath string) bool {
	if wtPath == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(wtPath, ".git"))
	return err == nil
}

// CaptureWorkspaceGit captures the non-recoverable git delta of wtPath: the
// current branch, a bounded bundle of the agent's local-only commits, and a
// single patch covering every uncommitted change. Returns (nil, nil) when
// wtPath is not a git worktree, so callers uniformly handle the
// non-git run-root by skipping the git portion of the snapshot.
func CaptureWorkspaceGit(ctx context.Context, wtPath string) (*GitDelta, error) {
	if !IsGitWorktree(wtPath) {
		return nil, nil
	}
	d := &GitDelta{}

	// HEAD commit SHA — always captured. Restore positions the worktree here,
	// and it's what lets restore recreate a never-pushed branch or check out a
	// detached HEAD when the bundle is empty. A failure here is a real anomaly
	// (a delegated worktree always has commits), so surface it rather than
	// snapshotting a workspace we can't rebuild.
	head, err := gitCapture(ctx, wtPath, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	d.Head = strings.TrimSpace(string(head))

	// rev-parse --abbrev-ref reports "HEAD" for a detached head; treat that as
	// "no branch" so restore checks out d.Head detached instead of trying to
	// check out a branch literally named HEAD.
	if out, err := gitCapture(ctx, wtPath, nil, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(string(out)); b != "HEAD" {
			d.Branch = b
		}
	}

	bundle, err := bundleLocalCommits(ctx, wtPath, d.Branch)
	if err != nil {
		return nil, err
	}
	d.Bundle = bundle

	patch, err := captureUncommitted(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	d.Patch = patch

	return d, nil
}

// bundleLocalCommits writes a `git bundle` of <rev> --not --remotes — the
// commits the agent made that aren't on any remote-tracking ref yet — and
// returns its bytes. Returns (nil, nil) when git refuses to create an empty
// bundle: the tip is already published, so there are no local-only commits to
// carry and the bare reproduces the committed state unaided.
func bundleLocalCommits(ctx context.Context, wtPath, branch string) ([]byte, error) {
	rev := branch
	if rev == "" {
		rev = "HEAD"
	}
	f, err := os.CreateTemp("", "tf-bundle-*.bundle")
	if err != nil {
		return nil, fmt.Errorf("bundle tempfile: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(name) }()

	if _, err := gitCapture(ctx, wtPath, nil, "bundle", "create", name, rev, "--not", "--remotes"); err != nil {
		if strings.Contains(err.Error(), "empty bundle") {
			return nil, nil
		}
		return nil, fmt.Errorf("git bundle: %w", err)
	}
	return os.ReadFile(name)
}

// captureUncommitted produces one binary patch of every uncommitted change in
// wtPath via a throwaway index seeded from HEAD: stage everything (add -A
// records modifications, deletions, and untracked additions alike), drop the
// managed _scratch tree, then diff that index against HEAD. Using
// GIT_INDEX_FILE keeps the worktree's real index untouched so a warm-path
// resume still sees the agent's staging exactly as it left it. Returns
// (nil, nil) for a clean tree.
//
// _scratch is removed from the staged set explicitly rather than left to the
// worktree's excludes: snapshot owns the _scratch capture separately (with its
// own entity-memory / project-knowledge exclusions), and a linked worktree's
// managed excludes live in the per-worktree gitdir while `add` consults the
// common dir — so relying on them here would leak _scratch into the patch.
func captureUncommitted(ctx context.Context, wtPath string) ([]byte, error) {
	idx, err := os.CreateTemp("", "tf-index-*")
	if err != nil {
		return nil, fmt.Errorf("temp index: %w", err)
	}
	idxName := idx.Name()
	_ = idx.Close()
	defer func() { _ = os.Remove(idxName) }()

	// Seed from gitBaseEnv() (not os.Environ()) so this throwaway-index env still
	// carries GIT_TERMINAL_PROMPT=0, keeping every capture on the package's one
	// base environment.
	env := append(gitBaseEnv(), "GIT_INDEX_FILE="+idxName)
	if _, err := gitCapture(ctx, wtPath, env, "read-tree", "HEAD"); err != nil {
		return nil, fmt.Errorf("seed temp index: %w", err)
	}
	if _, err := gitCapture(ctx, wtPath, env, "add", "-A"); err != nil {
		return nil, fmt.Errorf("stage worktree: %w", err)
	}
	// --ignore-unmatch: a no-op when _scratch was never staged (excludes did
	// hold, or there is no _scratch). The working-tree files stay on disk;
	// only the staged entries are dropped from the temp index.
	if _, err := gitCapture(ctx, wtPath, env, "rm", "-r", "--cached", "--ignore-unmatch", "--", "_scratch"); err != nil {
		return nil, fmt.Errorf("drop _scratch from temp index: %w", err)
	}
	patch, err := gitCapture(ctx, wtPath, env, "diff", "--cached", "--binary", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("diff temp index: %w", err)
	}
	if len(bytes.TrimSpace(patch)) == 0 {
		return nil, nil
	}
	return patch, nil
}

// RestoreWorkspaceGit rebuilds a worktree at wtDir from the durable bare clone
// plus the captured delta: ensure the bare exists, fold the bundled local-only
// commits into its branch ref, check out a fresh worktree at that branch,
// re-establish the managed _scratch excludes, and replay the uncommitted
// patch. It deliberately rebuilds rather than untarring a worktree so a
// snapshot taken on one host rehydrates cleanly on another — the worktree's
// `.git` pointer is host-specific and never travels.
//
// owner/repo locate the bare; cloneURL seeds it only when the bare is missing
// on this host (a fresh executor). In the local reboot / `/tmp`-wipe case the
// bare survives under the persistent state-root, so cloneURL goes unused.
//
// auth is the host-side HTTPS credential (inert in local/SSH/public). It
// authenticates both the on-demand re-clone of a missing bare AND the lazy
// promisor fetch the worktree-add checkout triggers on the blobless bare —
// without it, a fresh-executor resume of a private repo fails at either step
// with an anonymous "could not read Username" / promisor fetch error.
func RestoreWorkspaceGit(ctx context.Context, owner, repo, wtDir string, d *GitDelta, cloneURL string, auth CloneAuth) error {
	if d == nil {
		return fmt.Errorf("restore: nil git delta")
	}
	if d.Head == "" {
		return fmt.Errorf("restore: snapshot has no HEAD commit to check out")
	}
	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return err
	}
	switch _, statErr := os.Stat(bareDir); {
	case statErr == nil:
		// Bare present (the local reboot / `/tmp`-wipe case): reuse it.
	case os.IsNotExist(statErr):
		// Bare absent on this host (fresh executor). Seed it from the remote so
		// the bundle's prerequisite commits resolve. EnsureBareClone takes the
		// per-repo lock itself, so it runs outside the WithRepoLock below.
		if cloneURL == "" {
			return fmt.Errorf("restore: bare %s missing and no clone URL to seed it", bareDir)
		}
		if _, err := EnsureBareClone(ctx, owner, repo, cloneURL, WithCloneAuth(auth)); err != nil {
			return fmt.Errorf("restore: seed bare: %w", err)
		}
	default:
		// A non-"missing" stat error (permission, I/O) is a real problem —
		// surface it rather than masking it as a missing bare and re-cloning.
		return fmt.Errorf("restore: stat bare %s: %w", bareDir, statErr)
	}

	branch := d.Branch
	if err := WithRepoLock(owner, repo, func() error {
		// Clear any stale dir + worktree registration FIRST, so the branch ref
		// isn't "checked out" when we update it below (a surviving bare still
		// has the pre-loss worktree registered until this prune). Privileged
		// seam (see RemoveAt's doc): a stale dir surviving from a sandboxed
		// run is owned by the sandbox identity.
		_ = sandbox.RemoveRunTree(ctx, wtDir)
		if err := gitRunCtx(ctx, bareDir, "worktree", "prune"); err != nil {
			return fmt.Errorf("prune worktrees: %w", err)
		}

		// Get d.Head's objects into the bare and position the ref we'll check
		// out, across four cases (bundle present/absent × branch/detached):
		if len(d.Bundle) > 0 {
			// The bundle carries the agent's local-only commits; its
			// prerequisites are remote commits the (surviving or freshly
			// cloned) bare already has, so the fetch resolves.
			bname, cleanup, werr := writeTempBundle(d.Bundle)
			if werr != nil {
				return werr
			}
			defer cleanup()
			if branch != "" {
				// Force the branch ref to the bundled tip (and import objects).
				if err := gitRunCtx(ctx, bareDir, "fetch", bname,
					fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)); err != nil {
					return fmt.Errorf("unbundle into branch: %w", err)
				}
			} else if err := gitRunCtx(ctx, bareDir, "fetch", bname, "HEAD"); err != nil {
				// Detached: just import the objects; the detached add below
				// resolves d.Head directly.
				return fmt.Errorf("unbundle (detached): %w", err)
			}
		} else if branch != "" && !branchExists(bareDir, branch) {
			// No local-only commits, and the branch isn't in the bare (a fresh
			// clone, or a branch never pushed to a remote). An empty bundle
			// means d.Head is reachable from a remote ref, so it's already in
			// the bare — create the branch directly at the SHA rather than
			// guessing a refs/remotes/origin/<branch> that may not exist.
			if err := gitRunCtx(ctx, bareDir, "branch", branch, d.Head); err != nil {
				return fmt.Errorf("create branch at %s: %w", d.Head, err)
			}
		}

		if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
			return fmt.Errorf("mkdir runs parent: %w", err)
		}
		// gitRunCtxAuth on both adds: checking out onto the blobless bare
		// materializes deferred blobs via origin's promisor remote, so the lazy
		// fetch carries the credential on a private repo. The bundle fetch and
		// branch-create above stay unauth (local file / no network).
		if branch != "" {
			if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", wtDir, branch); err != nil {
				return fmt.Errorf("worktree add: %w", err)
			}
		} else if err := gitRunCtxAuth(ctx, bareDir, auth, "worktree", "add", "--detach", wtDir, d.Head); err != nil {
			// No branch: check out the exact commit detached, mirroring the
			// snapshotted detached HEAD.
			return fmt.Errorf("worktree add --detach: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	// Re-establish the managed _scratch excludes the fresh worktree lacks, then
	// replay the uncommitted changes on top of the committed branch state.
	if err := writeLocalExcludes(wtDir); err != nil {
		return fmt.Errorf("restore: write excludes: %w", err)
	}
	if len(d.Patch) > 0 {
		if err := applyPatch(ctx, wtDir, d.Patch); err != nil {
			return fmt.Errorf("restore: apply patch: %w", err)
		}
	}
	return nil
}

// writeTempBundle materializes bundle bytes to a temp file git can fetch from,
// returning the path and a cleanup func.
func writeTempBundle(bundle []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "tf-restore-*.bundle")
	if err != nil {
		return "", func() {}, fmt.Errorf("bundle tempfile: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(bundle); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", func() {}, fmt.Errorf("write bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", func() {}, fmt.Errorf("flush bundle: %w", err)
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// applyPatch replays an uncommitted-changes patch onto the freshly rebuilt
// worktree. --whitespace=nowarn keeps trailing-whitespace edits the agent made
// from tripping apply; --binary round-trips binary hunks. The changes land
// unstaged (file content is what matters for resume; the agent re-stages as it
// continues).
func applyPatch(ctx context.Context, wtDir string, patch []byte) error {
	f, err := os.CreateTemp("", "tf-patch-*.diff")
	if err != nil {
		return fmt.Errorf("patch tempfile: %w", err)
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.Write(patch); err != nil {
		_ = f.Close()
		return fmt.Errorf("write patch: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("flush patch: %w", err)
	}
	return gitRunCtx(ctx, wtDir, "apply", "--whitespace=nowarn", "--binary", name)
}

// ClaudeSessionPath returns the absolute path of the Claude Code session
// transcript (`<sessionID>.jsonl`) for an agent whose symlink-resolved cwd is
// resolvedCwd. Snapshot reads the transcript from it; rehydrate writes the
// carried transcript to the equivalent path under the new cwd so
// `claude --resume <sessionID>` reconnects after landing on a different
// worktree path. resolvedCwd must already be EvalSymlinks'd (see
// ResolveClaudeProjectCwd) because Claude Code keys the dir name off the
// resolved cwd.
func ClaudeSessionPath(resolvedCwd, sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("claude session path: empty session id")
	}
	home, err := claudeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolvedCwd), sessionID+".jsonl"), nil
}

// gitCapture runs git and returns its stdout (only) so a `git diff` /
// `git bundle` payload isn't corrupted by progress text on stderr — unlike
// gitOutputCtx, which combines the two. env, when non-nil, replaces the
// child's environment (used to point GIT_INDEX_FILE at a throwaway index).
//
// gitCapture runs git as the host (root, in multi mode) directly against the
// worktree, and captureUncommitted's `git add -A` / `git diff` go through here.
// Unlike the push-gate's metadata reads (a byte read of HEAD, and config read
// from OUTSIDE any repository — neither of which lets an agent-writable config
// run anything), add/diff DO consult the repository's own (agent-writable, once
// chowned to the sandbox uid) `.gitattributes` + `.git/config` to decide
// whether to invoke an external clean/smudge filter or diff driver. So this
// path must NOT be made ownership-tolerant against a chowned run root: doing so
// would let a compromised sandboxed agent plant both halves of that pair and
// get this host-side (root) capture to execute arbitrary code. Fixing the
// parking-snapshot failure against a chowned worktree needs the capture to run
// with no more privilege than the agent had (inside the jail), or to avoid
// filter-honoring git entirely — its own follow-up, not this.
func gitCapture(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Capture paths are local-only (rev-parse, bundle, diff against the
	// bare/worktree), but still run with prompts disabled for consistency: a
	// supplied env already starts from gitBaseEnv() (see captureUncommitted), and
	// a nil env falls back to it here.
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = gitBaseEnv()
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
