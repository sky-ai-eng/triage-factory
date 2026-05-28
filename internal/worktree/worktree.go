package worktree

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// claudeProjectsDir is where Claude Code auto-creates per-cwd session history.
const claudeProjectsDir = ".claude/projects"

// Per-repo mutexes prevent concurrent fetches from racing on the same bare repo.
var (
	repoMu    sync.Mutex
	repoLocks = map[string]*sync.Mutex{}
)

// onCloneResult is invoked by EnsureBareClone after every clone attempt
// (success or failure) for the per-repo callback wired in main.go. The
// callback writes to repo_profiles.clone_status / clone_error /
// clone_error_kind and broadcasts a websocket event so the Repos page
// updates live, and logs the failure to stderr so it's visible in
// `journalctl` / launch-agent logs even if the user isn't on the page.
//
// Package-level (rather than struct-level) because the worktree package
// has no struct — main.go calls EnsureBareClone / CreateForPR directly.
// One callback per process is enough; tests that don't set it get the
// nil-guard early-return.
var (
	onCloneResultMu sync.RWMutex
	onCloneResult   func(owner, repo string, err error)
)

// SetOnCloneResult installs the post-clone callback. Safe to call
// multiple times (last writer wins). Pass nil to detach (used by tests).
func SetOnCloneResult(cb func(owner, repo string, err error)) {
	onCloneResultMu.Lock()
	defer onCloneResultMu.Unlock()
	onCloneResult = cb
}

// fireCloneResult invokes the registered callback if any. Recovers from
// panics in the callback so a misbehaving consumer can't bring down a
// poller goroutine — the callback's job is purely observational.
func fireCloneResult(owner, repo string, err error) {
	onCloneResultMu.RLock()
	cb := onCloneResult
	onCloneResultMu.RUnlock()
	if cb == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[worktree] onCloneResult callback panicked for %s/%s: %v", owner, repo, r)
		}
	}()
	cb(owner, repo, err)
}

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

// WithRepoLock serializes a callback against the per-repo bare-clone
// mutex. Used by callers outside the worktree package — the
// pending-PR live-diff path in particular runs `git fetch` against
// the bare to sync the agent's pushed branch, which races with
// curator refresh / bootstrap / worktree creation if those are
// happening concurrently for the same repo. Concurrent fetches on a
// single bare can fail with "fatal: Unable to create
// '<bare>/refs/remotes/origin/<branch>.lock'" or otherwise corrupt
// the ref, hence the lock.
//
// Callback returns drive the caller's error path; the lock is
// always released on return.
func WithRepoLock(owner, repo string, fn func() error) error {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

const (
	reposDir = ".triagefactory/repos" // bare clones: ~/.triagefactory/repos/{owner}/{repo}.git
	runsDir  = "triagefactory-runs"   // worktrees: /tmp/triagefactory-runs/{run-id}
)

func repoDir(owner, repo string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, reposDir, owner, repo+".git"), nil
}

// RepoDir is the exported variant for callers outside the worktree
// package — specifically internal/server's livePRDiff path, which
// runs `git -C <bareDir> diff` against the bare clone for the
// pending-PR live diff. Wrapper rather than renaming repoDir to
// keep the existing internal call sites untouched.
func RepoDir(owner, repo string) (string, error) {
	return repoDir(owner, repo)
}

func runDir(runID string) string {
	return filepath.Join(os.TempDir(), runsDir, runID)
}

// RunRoot returns the run-root path for a given runID, without
// creating it. The Jira lazy-worktree CLI calls this from a delegated
// agent process to derive the parent directory under which `workspace
// add` materializes per-repo worktrees as `{runRoot}/{owner}/{repo}/`.
// Callers who need the directory to exist on disk should use MakeRunRoot
// instead.
func RunRoot(runID string) string {
	return runDir(runID)
}

// MakeRunRoot creates the run-root directory and returns its absolute
// path. Used by the spawner's setupJira path: the agent's initial cwd
// is the run-root (a throwaway dir holding only _scratch/ scratch
// subdirs until the agent calls `workspace add` to materialize
// worktrees as subdirs).
//
// Single-purpose vs. CreateForBranch: CreateForBranch creates a worktree
// AT runDir(runID); MakeRunRoot creates only the directory itself, with
// no git contents. The Jira lazy path uses MakeRunRoot so the agent has
// somewhere to land before it has chosen which repo(s) to materialize.
func MakeRunRoot(runID string) (string, error) {
	dir := runDir(runID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir run root: %w", err)
	}
	return dir, nil
}

// RemoveRunRoot removes the run-root directory and everything under it
// (including any per-repo worktrees that were materialized as subdirs).
// Safe if missing. Cleanup of the bare-side worktree registrations is
// handled by RemoveAt + pruneAll for each individual worktree before
// this is called; this is the final sweep of the parent dir itself.
func RemoveRunRoot(runID string) {
	os.RemoveAll(runDir(runID))
}

// MakeRunCwd creates a throwaway cwd for delegated runs that have no worktree.
// Vestigial — superseded by MakeRunRoot now that Jira runs always populate
// the run-root and materialize worktrees as subdirs. Kept temporarily so
// future cleanup callers don't break; remove in a follow-up once no callers
// remain.
func MakeRunCwd(runID string) (string, error) {
	dir := filepath.Join(os.TempDir(), runsDir, runID+"-nocwd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir run cwd: %w", err)
	}
	return dir, nil
}

// RemoveRunCwd removes the throwaway cwd created by MakeRunCwd. Safe if missing.
// Vestigial — see MakeRunCwd.
func RemoveRunCwd(runID string) {
	os.RemoveAll(filepath.Join(os.TempDir(), runsDir, runID+"-nocwd"))
}

// encodeClaudeProjectDir returns the directory name Claude Code uses
// under ~/.claude/projects/ for a symlink-resolved absolute cwd.
//
// Encoding rule (verified empirically against Claude Code 2.1.119):
// every '/' AND every '.' in the resolved path becomes '-'. The
// dot-replacement is the part that's easy to miss — paths like
// ~/.triagefactory/... contain dots, and only replacing slashes
// produces a name Claude Code can't find. We discovered this when
// `claude --resume <id>` from a takeover dir reported "No
// conversation found": our materialized JSONL was at
// `-Users-...-.triagefactory-takeovers-run-<id>` while Claude looked
// at `-Users-...--triagefactory-takeovers-run-<id>` (note the `--`
// where the dot got collapsed).
//
// Caveat: only `/` and `.` are verified. Claude Code may also rewrite
// other characters (underscores, spaces). The paths Triage Factory
// actually uses (/tmp/triagefactory-runs/<uuid> and
// ~/.triagefactory/takeovers/run-<uuid>) only contain slashes and
// dots, so this matches in practice; if takeover_dir is ever
// configured to a path with other special characters, revisit.
func encodeClaudeProjectDir(resolvedAbs string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(resolvedAbs)
}

// EncodeClaudeProjectDir exposes Claude Code's cwd-encoding rule to
// non-worktree packages that need to locate session artifacts (for
// example project bundle import/export). Input must already be the
// symlink-resolved absolute cwd.
func EncodeClaudeProjectDir(resolvedAbs string) string {
	return encodeClaudeProjectDir(resolvedAbs)
}

// claudeProjectEncoding combines symlink resolution and encoding for
// callers that have an unresolved cwd. Returns the encoded name and
// the resolved path. On EvalSymlinks failure (typically because the
// path doesn't exist anymore), falls back to the input — Claude Code
// would have used the resolved path while it was running, so callers
// that need the canonical encoding for an extant path should resolve
// before the path goes away.
func claudeProjectEncoding(cwd string) (encoded, resolved string) {
	resolved = cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		resolved = r
	}
	return encodeClaudeProjectDir(resolved), resolved
}

// MaterializeSessionForTakeover copies the Claude Code session JSONL
// from the agent's original ~/.claude/projects entry into the takeover
// destination's project entry, so `claude --resume <id>` works when
// the user runs it from the takeover dir.
//
// Claude Code keys session storage by encoded cwd: the agent ran at
// oldCwd, so its conversation lives at
// ~/.claude/projects/<encoded-oldCwd>/<sessionId>.jsonl. The user's
// resume runs from newCwd, where Claude Code looks under
// ~/.claude/projects/<encoded-newCwd>/. Without copying the JSONL
// across, the resume fails with "No conversation found with session
// ID" — empirically observed.
//
// oldCwd MUST be resolved (EvalSymlinks'd) before the source worktree
// gets moved/removed, otherwise the symlink resolution would fail.
// Callers capture this in Spawner.Takeover before CopyForTakeover
// runs. newCwd is the live takeover destination and gets resolved
// here.
//
// Returns an error if the source JSONL doesn't exist or the copy
// fails — both are conditions that would leave the user unable to
// resume, so we want them surfaced loudly rather than silently
// degrading.
func MaterializeSessionForTakeover(resolvedOldCwd, newCwd, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("materialize session: empty session id")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("materialize session: %w", err)
	}

	oldEncoded := encodeClaudeProjectDir(resolvedOldCwd)
	newEncoded, _ := claudeProjectEncoding(newCwd)

	src := filepath.Join(home, claudeProjectsDir, oldEncoded, sessionID+".jsonl")
	destDir := filepath.Join(home, claudeProjectsDir, newEncoded)
	dest := filepath.Join(destDir, sessionID+".jsonl")

	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("materialize session: source JSONL at %s: %w", src, err)
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return fmt.Errorf("materialize session: mkdir %s: %w", destDir, err)
	}
	if err := copyFile(src, dest, 0600); err != nil {
		return fmt.Errorf("materialize session: copy %s -> %s: %w", src, dest, err)
	}
	log.Printf("[worktree] materialized session %s for takeover (%s -> %s)", sessionID, src, dest)
	return nil
}

// ResolveClaudeProjectCwd returns the symlink-resolved absolute path
// the way Claude Code records cwds for project-dir naming. Spawner.
// Takeover captures this for the source worktree BEFORE the move/
// overlay removes the path; passing the resolved value to
// MaterializeSessionForTakeover later is what makes the JSONL copy
// find the right source.
func ResolveClaudeProjectCwd(cwd string) string {
	_, resolved := claudeProjectEncoding(cwd)
	return resolved
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
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.RemoveAll(projectDir); err != nil {
		log.Printf("[worktree] remove ghost project dir %s: %v", projectDir, err)
	}
}

// RemoveClaudeProjectDirUnderTakeover is the takeover-base counterpart of
// RemoveClaudeProjectDir. The normal-completion path's TMPDIR rail correctly
// rejects paths under ~/.triagefactory/takeovers/, so the release path needs
// its own helper with the matching prefix check.
//
// Safety rail: refuses to act unless the resolved cwd lives under
// resolvedTakeoverBase. takeoverBase may be a configured override
// (ServerConfig.TakeoverDir). Both cwd and takeoverBase are made absolute
// (filepath.Abs) and symlink-resolved (filepath.EvalSymlinks) before the
// prefix check, so callers can pass relative paths, "~"-prefixed paths
// already expanded by ResolvedTakeoverDir, or fully-resolved paths
// without changing the result.
func RemoveClaudeProjectDirUnderTakeover(cwd, takeoverBase string) {
	if cwd == "" || takeoverBase == "" {
		return
	}

	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return
	}
	resolved, err := filepath.EvalSymlinks(cwdAbs)
	if err != nil {
		return
	}

	baseAbs, err := filepath.Abs(takeoverBase)
	if err != nil {
		return
	}
	baseResolved, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return
	}

	rel, err := filepath.Rel(baseResolved, resolved)
	if err != nil {
		log.Printf("[worktree] refusing to clean project dir for cwd with unrelatable path: %s (base %s)", resolved, baseResolved)
		return
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		log.Printf("[worktree] refusing to clean project dir for cwd outside takeover base: %s (base %s)", resolved, baseResolved)
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.RemoveAll(projectDir); err != nil {
		log.Printf("[worktree] remove takeover project dir %s: %v", projectDir, err)
	}
}

// RemoveClaudeProjectDirForResolved removes ~/.claude/projects/<encoded>
// for a cwd whose absolute, symlink-resolved path is already known. Use
// this when the cwd may no longer exist on disk (e.g. RemoveAt has
// already destroyed the worktree), since the standard
// RemoveClaudeProjectDir / RemoveClaudeProjectDirUnderTakeover variants
// EvalSymlinks the cwd and silently no-op on a non-existent path. The
// caller is responsible for any safety-rail check — by the time you have
// a resolved path, you've already validated it.
//
// Best-effort, like the other variants: failures are logged. The
// resolved path must already be the symlink-evaluated form (e.g.
// /private/var/... rather than /var/...) because Claude Code keys the
// projects-dir name off the resolved cwd at the time the agent ran.
func RemoveClaudeProjectDirForResolved(resolvedCwd string) {
	if resolvedCwd == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolvedCwd))
	if err := os.RemoveAll(projectDir); err != nil {
		log.Printf("[worktree] remove project dir %s: %v", projectDir, err)
	}
}

// EnsureBareClone is the exported entry point for callers that want a
// bare clone of owner/repo materialized. It's idempotent: if the bare
// already exists, it only repairs a drifted origin URL; otherwise it
// clones the bare and then repairs origin if needed. Bootstrap calls
// this for every configured repo on startup so first-delegation
// latency disappears.
//
// The cloneURL must be the upstream repository's URL (the URL stored
// in repo_profiles.clone_url, populated during repo profiling). Passing
// a fork's URL would clobber the bare's origin and is the historical
// bug this function exists to prevent — see repairOriginURL.
func EnsureBareClone(ctx context.Context, owner, repo, cloneURL string) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()
	return ensureBareCloneLocked(ctx, owner, repo, cloneURL)
}

// ensureBareCloneLocked clones the bare if missing and repairs a
// drifted origin URL when one is configured. Caller must hold the
// per-repo lock.
//
// The clone-if-missing step is split into a separate helper so the
// post-clone configuration (the URL repair) runs whether or not the
// bare already existed. A repo whose bare was created before this
// code shipped may have origin pointed at a fork (the historical
// bug); calling this on bootstrap corrects it.
//
// We deliberately do NOT add a global PR fetch refspec
// (`+refs/pull/*/head:refs/remotes/pr/*`) to the bare. CreateForPR
// already fetches the specific PR ref it needs via an explicit
// refspec. A configured global refspec only kicks in for bare
// `git fetch` / `git pull`, where it would mirror every PR's head
// on every refresh — thousands of extra refs on busy repos for no
// internal benefit.
func ensureBareCloneLocked(ctx context.Context, owner, repo, cloneURL string) (bareDir string, err error) {
	// Fire the post-clone callback exactly once per call so consumers
	// (main.go's hook → repo_profiles + websocket) see one event per
	// attempt regardless of whether we hit the fresh-clone branch or
	// the existing-bare repair branch. Logging the failure here
	// synchronously guarantees stderr coverage even if no callback is
	// wired (tests, future CLI tools, etc.).
	//
	// The callback ITSELF runs in a goroutine because the per-repo
	// mutex acquired by EnsureBareClone / CreateForPR /
	// createBranchWorktreeAt is still held when this defer fires
	// (their `defer mu.Unlock()` runs after this defer in their own
	// frames). main.go's callback can do up to ~15s of work on the
	// failure path (DB write + WS broadcast + a bounded-by-15s SSH
	// preflight to classify the failure kind). Holding the per-repo
	// lock through that would block concurrent worktree operations
	// and the live-PR-diff endpoint for the same repo. The callback
	// is observability-only — no caller depends on it completing
	// before they see results — and fireCloneResult recovers panics,
	// so detaching it is safe.
	defer func() {
		if err != nil {
			log.Printf("[worktree] ensureBareClone %s/%s: %v", owner, repo, err)
		}
		go fireCloneResult(owner, repo, err)
	}()

	bareDir, err = cloneBareIfMissing(ctx, owner, repo, cloneURL)
	if err != nil {
		return "", err
	}
	if cloneURL != "" {
		if err = repairOriginURL(ctx, bareDir, cloneURL); err != nil {
			return "", fmt.Errorf("repair origin url: %w", err)
		}
	}
	return bareDir, nil
}

// cloneBareIfMissing performs the actual `git clone --bare` only when
// the bare directory doesn't yet exist. Caller must hold the per-repo
// lock. Does NOT configure origin URL or refspecs — see
// ensureBareCloneLocked for the full lifecycle.
func cloneBareIfMissing(ctx context.Context, owner, repo, cloneURL string) (string, error) {
	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}

	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		if cloneURL == "" {
			return "", fmt.Errorf("bare clone for %s/%s missing and no cloneURL provided", owner, repo)
		}
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

// repairOriginURL sets remote.origin.url to wantURL when it differs
// from the currently-configured value. Idempotent: returns immediately
// when the URL already matches.
//
// This corrects the historical bug where a fork PR encountered before
// the upstream itself caused the bare's origin to point at the fork
// (the spawner used to pass pr.CloneURL — the head's URL — into the
// initial clone). Calling EnsureBareClone with the upstream URL fixes
// the drift on next bootstrap.
func repairOriginURL(ctx context.Context, bareDir, wantURL string) error {
	currentURL, err := gitOutputCtx(ctx, bareDir, "config", "--get", "remote.origin.url")
	if err != nil {
		// No origin configured (or read failed). Recreate the remote when
		// it's missing; if it already exists but the config lookup failed,
		// fall back to updating its URL in place.
		if addErr := gitRunCtx(ctx, bareDir, "remote", "add", "origin", wantURL); addErr == nil {
			return nil
		}
		return gitRunCtx(ctx, bareDir, "remote", "set-url", "origin", wantURL)
	}
	currentURL = strings.TrimSpace(currentURL)
	if currentURL == wantURL {
		return nil
	}
	log.Printf("[worktree] repairing origin url for %s: %q -> %q", bareDir, currentURL, wantURL)
	return gitRunCtx(ctx, bareDir, "remote", "set-url", "origin", wantURL)
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
func CreateForPR(ctx context.Context, owner, repo, upstreamCloneURL, headCloneURL, headBranch string, prNumber int, runID string) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, upstreamCloneURL)
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
	start := time.Now()
	if err := gitRunCtx(ctx, bareDir, "fetch", "origin", branchRef); err != nil {
		return "", fmt.Errorf("fetch PR #%d head into %s: %w", prNumber, localBranch, err)
	}
	log.Printf("[worktree] fetch PR #%d (refs/pull/%d/head -> %s) completed in %s", prNumber, prNumber, localBranch, time.Since(start).Round(time.Millisecond))

	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}

	// Pass the bare branch name (not refs/heads/<name>) so git
	// attaches the worktree to the local branch instead of going
	// detached. `git worktree add <path> refs/heads/<name>` treats
	// the ref path as a commit-ish and detaches; `git worktree add
	// <path> <name>` resolves it as a branch and attaches.
	if err := gitRunCtx(ctx, bareDir, "worktree", "add", wtDir, localBranch); err != nil {
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
		log.Printf("[worktree] PR #%d head repository is unavailable (deleted fork); worktree is read-only", prNumber)
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

	log.Printf("[worktree] PR worktree at %s (local branch: %s, head: %s, fork: %v)", wtDir, localBranch, headBranch, isFork)
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
		log.Printf("[worktree] rollback PR setup: remove worktree: %v", rmErr)
	}

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
// fork-only sweep would otherwise miss after a takeover dir is
// destroyed. The value is the PR number — preserved as the source
// of truth for the head-<n> remote name when one exists.
//
// Git lower-cases config variable names internally, so the regex
// in the sweep matches against `tfprnumber` even though we set
// `tfPRNumber` here. Using the lowercase form everywhere keeps the
// match logic obvious.
const trackedBranchMarkerKey = "tfprnumber"

// cleanupTimeout caps the time CleanupPRConfig and SweepStaleForkPRConfig
// will spend on their detached-context git invocations. Reclamation is
// best-effort; if a single config-rewrite hangs (locked file, slow disk),
// we'd rather time out than block run finalization indefinitely.
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
// branch checked out by any live worktree, so a takeover dir or
// concurrent delegation would force this to no-op rather than
// silently break a live checkout.
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
//   - Run was taken over: the runAgent defer's wasTakenOver gate
//     skips cleanup so the user's takeover dir can keep using its
//     tracking config for push. Once the takeover dir is destroyed,
//     this sweep reclaims the leak on the next bootstrap pass —
//     covers both fork PRs (head-<n>) and own-repo PRs
//     (branch.<headRef>.*).
//   - Run was cancelled at a layer above the runAgent defer (rare):
//     inline cleanup never runs.
//
// Walking markers (rather than head-<n> remotes alone) is what makes
// this cover own-repo PRs too — fork PRs have a head-<n> the old
// approach could find, but own-repo PRs have only the branch config
// block, which the marker exposes generically.
//
// Safe to call while takeovers are still in use because `git worktree
// list` reports them and we only reclaim branches with no live
// checkout. Best-effort: orphan-detection failures or partial
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
		log.Printf("[worktree] swept %d stale per-PR config block(s) from %s", reclaimed, bareDir)
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
// this block after a takeover dir is destroyed without the spawner's
// inline cleanup (which the wasTakenOver gate skips).
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

// CreateForBranch sets up a worktree on a new feature branch based off
// a given base, at the run's default location runDir(runID). If
// baseBranch is empty, the repo's default branch is detected from
// origin/HEAD. Used by the eager GitHub PR delegation path where the
// run has exactly one repo.
func CreateForBranch(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID string) (string, error) {
	wtDir, err := makeWorktreeDir(runID)
	if err != nil {
		return "", err
	}
	return createBranchWorktreeAt(ctx, owner, repo, cloneURL, baseBranch, featureBranch, runID, wtDir)
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
	return createBranchWorktreeAt(ctx, owner, repo, cloneURL, baseBranch, featureBranch, runID, wtDir)
}

// createBranchWorktreeAt is the shared body of the two CreateForBranch
// variants — bare-clone setup, base-branch fetch, `git worktree add`
// (with branchExists reattach), and exclude-or-rollback. The two
// public callers differ only in where wtDir lives on disk.
func createBranchWorktreeAt(ctx context.Context, owner, repo, cloneURL, baseBranch, featureBranch, runID, wtDir string) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()

	bareDir, err := ensureBareCloneLocked(ctx, owner, repo, cloneURL)
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
	if err := gitRunCtx(ctx, bareDir, "fetch", "origin", baseRef); err != nil {
		return "", fmt.Errorf("fetch base branch %s: %w", baseBranch, err)
	}
	log.Printf("[worktree] fetch %s completed in %s", baseBranch, time.Since(start).Round(time.Millisecond))

	// Create worktree — reuse the branch if it already exists (re-delegation),
	// otherwise create a new one off the just-fetched remote-tracking ref.
	if branchExists(bareDir, featureBranch) {
		// Branch exists from a previous run — check it out
		if err := gitRunCtx(ctx, bareDir, "worktree", "add", wtDir, featureBranch); err != nil {
			return "", fmt.Errorf("worktree add existing branch: %w", err)
		}
	} else {
		if err := gitRunCtx(ctx, bareDir, "worktree", "add", "-b", featureBranch, wtDir, remoteRef); err != nil {
			return "", fmt.Errorf("worktree add new branch: %w", err)
		}
	}

	if err := addExcludesOrRollback(runID, wtDir); err != nil {
		return "", err
	}

	log.Printf("[worktree] branch worktree at %s (%s from %s)", wtDir, featureBranch, baseBranch)
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
			log.Printf("[worktree] rollback after exclude-write failure: %v", rmErr)
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

// CopyForTakeover materializes a working copy of a delegated run's
// worktree at <baseDir>/run-<runID>/ so the user can resume the headless
// Claude Code session interactively. Returns the absolute destination
// path on success.
//
// The destination is a *linked worktree* of the same bare repo the
// original run used — the same lightweight pattern CreateForPR /
// CreateForBranch already use. `.git` in the destination is a 1-line
// pointer file referencing <bareDir>/worktrees/run-<runID>/, NOT a
// full git directory. Trade-offs vs. a full clone:
//
//   - Near-instant. No object copying, no blob materialization, no
//     network. For a multi-GB repo the difference is "minutes" vs.
//     "milliseconds." This is the whole point of the change.
//   - Full git functionality: status, diff, log, commit, push to the
//     bare's origin (i.e. GitHub) all work normally, because the bare
//     repo is the worktree's gitdir.
//   - Coupled to TF's repo cache: if the user wipes
//     ~/.triagefactory/repos, the takeover dir breaks. The modal
//     surfaces this so the user can `git clone` to detach if they want
//     a fully standalone copy.
//
// We add the linked worktree on the SAME branch the agent was using so
// the user can continue and push back without extra steps. This is
// safe because Spawner.Takeover removes the original /tmp worktree
// before returning — git allows multiple worktrees on the same branch
// only one at a time, but the original is gone by the time the user
// touches the takeover dir.
//
// `--no-checkout` skips materializing files from HEAD (which would
// trigger lazy blob fetching from the partial-clone bare). We then
// overlay the agent's working-tree state from the source worktree —
// modified, added, and untracked files, minus the managed scratch
// directories — so the user sees exactly what the agent was looking
// at when takeover happened.
//
// Files matching managedExcludePatterns (_scratch/) are not copied.
// The destination's .git/info/exclude inherits the bare's
// configuration; we re-write our managed block so those paths stay
// hidden from `git status` in the takeover dir as well.
func CopyForTakeover(ctx context.Context, runID, srcWorktree, baseDir string) (string, error) {
	if runID == "" {
		return "", fmt.Errorf("takeover: empty run id")
	}
	if srcWorktree == "" {
		return "", fmt.Errorf("takeover: empty source worktree path")
	}
	// Reject empty baseDir explicitly. filepath.Abs("") silently
	// returns the binary's current working directory, so a caller
	// that bypassed Spawner.Takeover's validation would create
	// takeovers wherever the user launched the binary from — bad
	// surprise for anything from /usr/local/bin to ~/Downloads.
	// Spawner.Takeover catches this before us, but the helper
	// shouldn't trust the upstream.
	if baseDir == "" {
		return "", fmt.Errorf("takeover: empty destination base dir")
	}

	// Resolve to an absolute path before doing anything else. The
	// returned path ends up in the resume command shown to the user;
	// if takeover_dir is configured as a relative path, a relative
	// destination would only work if the user pasted the command from
	// the same cwd the binary was launched from. Make it cwd-independent.
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("takeover: resolve base dir: %w", err)
	}
	destDir := filepath.Join(absBase, "run-"+runID)
	if err := os.MkdirAll(filepath.Dir(destDir), 0755); err != nil {
		return "", fmt.Errorf("takeover: mkdir base: %w", err)
	}
	// Check destination existence BEFORE touching the source. Re-takeover
	// for the same runID must be idempotent: the move path renames the
	// source out of /tmp on the first call, so a naive second call would
	// fail validating the now-missing source even though the work is
	// already done.
	if _, err := os.Stat(destDir); err == nil {
		// A previous takeover for the same run was already materialized
		// — IF the existing path is actually a usable worktree. Validate
		// it before returning. A regular file at this path (typo, errant
		// `touch`), an empty directory left behind by an interrupted
		// previous attempt, or a directory whose `.git` pointer
		// references a gitdir that no longer exists would all "succeed"
		// the bare stat check but be useless for resume — the user
		// would `cd` in and find nothing works.
		if err := validateExistingTakeoverDest(ctx, destDir); err != nil {
			return "", fmt.Errorf("takeover: existing destination at %s is not a usable worktree (%w); remove or rename it and retry", destDir, err)
		}
		// Returning the existing path is the right call here — re-
		// invoking the endpoint shouldn't clobber a working copy the
		// user may already have local edits in.
		return destDir, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("takeover: stat destination: %w", err)
	}

	srcInfo, err := os.Stat(srcWorktree)
	if err != nil {
		return "", fmt.Errorf("takeover: stat source worktree: %w", err)
	}
	if !srcInfo.IsDir() {
		return "", fmt.Errorf("takeover: source worktree is not a directory: %s", srcWorktree)
	}

	bareDir, err := resolveBareDirFromWorktree(srcWorktree)
	if err != nil {
		return "", fmt.Errorf("takeover: locate bare repo: %w", err)
	}

	branch, err := currentBranch(ctx, srcWorktree)
	if err != nil {
		return "", fmt.Errorf("takeover: read current branch: %w", err)
	}

	// Fast path: when source and destination are on the same filesystem,
	// `git worktree move` is an atomic rename(2) and lets us avoid the
	// overlay copy entirely. The branch registration moves with the
	// worktree so there's no co-checkout situation to bypass with --force.
	moved, err := tryMoveWorktree(ctx, bareDir, srcWorktree, destDir)
	if err != nil {
		return "", fmt.Errorf("takeover: move worktree: %w", err)
	}
	if moved {
		// _scratch/ traveled with the move; the overlay path skips it
		// explicitly because it's TF infra and shouldn't land in the
		// user's hands. Mirror that here so behavior is the same
		// regardless of which path we took.
		_ = os.RemoveAll(filepath.Join(destDir, "_scratch"))
		// Refresh the managed exclude block in case managedExcludePatterns
		// has grown since the agent originally wrote it. Best-effort.
		if err := writeLocalExcludes(destDir); err != nil {
			log.Printf("[worktree] warning: takeover %s: refresh excludes after move: %v", runID, err)
		}
		log.Printf("[worktree] takeover via move: %s -> %s (branch %s)", srcWorktree, destDir, branch)
		return destDir, nil
	}

	// Fallback: cross-filesystem move isn't supported by git (rename(2)
	// returns EXDEV). Add a fresh linked worktree at the destination and
	// overlay the agent's working tree. Slower than the rename path but
	// correctness-equivalent for users whose $TMPDIR and takeover_dir
	// live on different filesystems.
	if err := addAndOverlayForTakeover(ctx, runID, bareDir, srcWorktree, destDir, branch); err != nil {
		return "", err
	}

	// The overlay path leaves the original worktree in place because we
	// needed its files for the copy. Now that the copy is done, remove
	// the source worktree dir explicitly using its actual path —
	// callers can pass an arbitrary srcWorktree, so deriving the path
	// from runID via Remove() would risk removing the wrong directory
	// if the source ever differs from runDir(runID). (The move path
	// doesn't need this — git already renamed the source out from
	// under us.)
	if err := RemoveAt(srcWorktree, runID); err != nil {
		log.Printf("[worktree] warning: takeover %s: remove original after overlay: %v", runID, err)
	}

	log.Printf("[worktree] takeover via overlay: %s -> %s (branch %s)", srcWorktree, destDir, branch)
	return destDir, nil
}

// validateExistingTakeoverDest checks that a path which already exists
// at the takeover destination is healthy enough to resume from.
// "Healthy" means: it's a directory, and `git rev-parse --git-dir`
// succeeds inside it. The git probe is cheap (sub-millisecond) and
// catches the cases a bare os.Stat doesn't:
//
//   - Path is a regular file (returned because os.Stat doesn't care).
//     User would `cd` into nothing.
//   - Path is an empty directory left over from an interrupted previous
//     takeover (stat passes but there's no git metadata).
//   - Path has a `.git` pointer file referencing a gitdir that no
//     longer exists (user wiped ~/.triagefactory/repos, or the bare
//     was deleted). Pointer parses fine, but git ops fail.
//
// On any of these we return an error so CopyForTakeover surfaces a
// clear message asking the user to remove or rename the existing
// path. We deliberately don't auto-recover by deleting — the user may
// have intentionally placed something there (e.g., manually inspected
// a previous takeover), and silent destruction would be hostile.
func validateExistingTakeoverDest(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if _, err := gitOutputCtx(ctx, path, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("not a git worktree: %w", err)
	}
	return nil
}

// tryMoveWorktree attempts `git worktree move <src> <dest>` and returns
// (true, nil) on success, (false, nil) when the move is unsupported
// because src and dest are on different filesystems (git uses rename(2)
// which returns EXDEV cross-fs and refuses to fall back), and
// (false, err) for any other failure.
//
// The same-filesystem check is done up front via syscall.Stat_t.Dev so
// we don't have to parse git's stderr for "Invalid cross-device link"
// strings that vary across platforms and locales. The check looks at
// the parents of src and dest because dest doesn't exist yet — its
// device id is determined by the filesystem its parent lives on.
func tryMoveWorktree(ctx context.Context, bareDir, src, dest string) (bool, error) {
	sameFS, err := sameFilesystem(src, filepath.Dir(dest))
	if err != nil {
		// Stat of either path failed (src missing, dest parent missing).
		// Surface as a real error rather than silently falling through —
		// the overlay path would hit the same problem.
		return false, fmt.Errorf("same-fs check: %w", err)
	}
	if !sameFS {
		return false, nil
	}
	if err := gitRunCtx(ctx, bareDir, "worktree", "move", src, dest); err != nil {
		return false, fmt.Errorf("git worktree move: %w", err)
	}
	return true, nil
}

// sameFilesystem returns true iff a and b live on the same filesystem,
// determined by syscall.Stat_t.Dev. Used by the takeover-move pre-flight
// check; rename(2) refuses cross-device moves and we'd rather know up
// front than parse a platform-specific error message.
func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}

// addAndOverlayForTakeover is the cross-filesystem fallback for
// CopyForTakeover. Adds a fresh linked worktree at destDir (with --force
// because the source is still registered on the same branch) and
// overlays the agent's working tree on top. Caller is responsible for
// removing the source worktree afterward.
func addAndOverlayForTakeover(ctx context.Context, runID, bareDir, srcWorktree, destDir, branch string) error {
	// Linked-worktree add against the same bare. --no-checkout means git
	// only writes the gitdir + .git pointer; the working tree starts
	// empty and we fill it via overlayWorkingTree. Without --no-checkout
	// git would lazy-fetch blobs from the partial-clone bare, which is
	// the slow path we're explicitly avoiding.
	//
	// --force bypasses git's "branch is already checked out at <path>"
	// safety check. The original /tmp worktree IS still registered with
	// the bare repo at this point: Spawner.Takeover SIGKILLs the agent
	// before calling us but defers the worktree.Remove until after this
	// returns (we need the source files for overlayWorkingTree). Without
	// --force, git would refuse the add because the branch is co-checked
	// out by the original. The safety the flag bypasses — "don't let two
	// live worktrees fight over a branch" — doesn't apply here: the
	// agent is dead, no process is writing to the original, and the
	// dual-worktree state lasts only as long as the overlay copy.
	args := []string{"worktree", "add", "--force", "--no-checkout"}
	if branch != "" {
		args = append(args, destDir, branch)
	} else {
		// Detached HEAD on the source — give the destination a detached
		// HEAD too. We pass the source's HEAD commit explicitly because
		// `git worktree add --detach` without a commit-ish falls back
		// to the bare's HEAD, which may not resolve to a valid commit
		// for our partial-clone bare. Resolving from the source is
		// always correct: that's the commit the agent was sitting on.
		commitOut, err := gitOutputCtx(ctx, srcWorktree, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("takeover: resolve source HEAD: %w", err)
		}
		commit := strings.TrimSpace(commitOut)
		if commit == "" {
			return fmt.Errorf("takeover: source HEAD resolved to empty commit")
		}
		args = append(args, "--detach", destDir, commit)
	}
	if err := gitRunCtx(ctx, bareDir, args...); err != nil {
		return fmt.Errorf("takeover: worktree add: %w", err)
	}

	// Re-apply the managed exclude block (_scratch/) so `git status` in
	// the takeover dir doesn't surface our infra dirs even if the user
	// ends up needing them. Best-effort: a failure here is annoying but
	// not fatal — git status will just be a bit noisy.
	if err := writeLocalExcludes(destDir); err != nil {
		log.Printf("[worktree] warning: takeover %s: write excludes: %v", runID, err)
	}

	if err := overlayWorkingTree(srcWorktree, destDir); err != nil {
		return fmt.Errorf("takeover: overlay working tree: %w", err)
	}

	return nil
}

// resolveBareDirFromWorktree returns the bare repo behind a linked
// worktree by asking git directly via `rev-parse --git-common-dir` —
// git's canonical answer to "where is the shared object/refs store"
// for both linked worktrees and plain checkouts. Avoids parsing the
// `.git` pointer file ourselves, which would couple us to git's on-
// disk layout (`.git/worktrees/<name>/`) that may shift between git
// versions.
func resolveBareDirFromWorktree(wtDir string) (string, error) {
	commonDir, err := gitOutput(wtDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("rev-parse --git-common-dir: %w", err)
	}
	commonDir = strings.TrimSpace(commonDir)
	if commonDir == "" {
		return "", fmt.Errorf("git-common-dir returned empty")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(wtDir, commonDir)
	}
	abs, err := filepath.Abs(commonDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("git common dir %s does not exist: %w", abs, err)
	}
	return abs, nil
}

func currentBranch(ctx context.Context, wtDir string) (string, error) {
	out, err := gitOutputCtx(ctx, wtDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "HEAD" {
		// Detached HEAD — return empty so the caller can detect the
		// detached state and handle it explicitly (for example by resolving
		// the source HEAD commit), rather than relying on a clone default.
		return "", nil
	}
	return branch, nil
}

// overlayWorkingTree copies every file from src onto dest, skipping the
// `.git` directory (the destination already has its own git metadata
// from the linked-worktree add) and skipping managedExcludePatterns
// (those are TF infrastructure dirs, not user-relevant state).
//
// Because the destination was created with `git worktree add
// --no-checkout`, its working tree starts empty. After this overlay
// runs, the working tree contains exactly what the agent's worktree
// contained — no more, no less. From git's POV: files modified in src
// show as modified in dest, untracked files in src show as untracked
// in dest, and files the agent deleted (present at HEAD but absent
// from src) show as deleted in dest. Deletions are represented
// implicitly because we never put them in dest in the first place.
func overlayWorkingTree(src, dest string) error {
	skipNames := map[string]bool{".git": true}
	for _, pat := range managedExcludePatterns {
		// managedExcludePatterns end with '/' — strip for filename match
		skipNames[strings.TrimSuffix(pat, "/")] = true
	}

	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}

	return filepath.Walk(srcAbs, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// A file vanishing between readdir and lstat (e.g. the agent
			// or a child subprocess flushing writes during the brief
			// window before SIGKILL is reaped) is recoverable — we
			// just skip the entry and continue. Anything else is fatal.
			if os.IsNotExist(walkErr) {
				log.Printf("[worktree] takeover overlay: skipping vanished entry %s", path)
				return nil
			}
			return walkErr
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Top-level skips: .git, _scratch
		topSegment := rel
		if i := strings.Index(rel, string(filepath.Separator)); i >= 0 {
			topSegment = rel[:i]
		}
		if skipNames[topSegment] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		destPath := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(destPath, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			_ = os.Remove(destPath)
			return os.Symlink(target, destPath)
		}
		if err := copyFile(path, destPath, info.Mode().Perm()); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return nil
	})
}

// copyFile copies a regular file from src to dst, preserving the mode.
// Deliberately does NOT call out.Sync(): per-file fsync would serialize
// with disk on every iteration of the overlay walk (thousands of small
// files for a typical repo) for no durability benefit in this context.
// The takeover destination is a "user-about-to-use-this" workspace, not
// a crash-recovery target — a kernel crash mid-overlay would just have
// the user re-run takeover. Normal page-cache write-back is sufficient,
// and matches the move path which doesn't fsync either (rename(2) is
// atomic for the metadata, but the data writes the original agent did
// were never per-file fsync'd either).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// WorktreeBranch returns the name of the branch checked out at the given
// worktree path. Empty result + non-nil error means the path isn't a
// worktree (or git refused). The caller uses this to learn the local
// branch name without consulting the GitHub API — important for the
// release path, which has to reclaim the bare's branch ref before the
// fetch-into-checked-out-branch error can clear, and a network call
// would make release fail when GitHub is unreachable.
func WorktreeBranch(path string) (string, error) {
	out, err := gitOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(out)
	if branch == "HEAD" {
		// Detached HEAD — no branch name to clean up.
		return "", nil
	}
	return branch, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	return gitOutputCtx(context.Background(), dir, args...)
}

func gitOutputCtx(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("cancelled")
		}
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// RemoveAt removes a worktree directory by path and prunes the bare's
// stale registration. Callers pass the actual path rather than
// deriving it from a runID — every previous "Remove(runID)" caller
// had the path in scope already, and the runID-only convenience was
// a footgun: it silently targeted runDir(runID) regardless of where
// the worktree actually lived, which broke CopyForTakeover when the
// source was passed in explicitly.
//
// runID is used only for the log line; pass "" if not available.
func RemoveAt(path, runID string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove worktree dir %s: %w", path, err)
	}

	// Prune stale worktree refs from all bare repos
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pruneAll(filepath.Join(home, reposDir))

	if runID != "" {
		log.Printf("[worktree] removed %s (%s)", runID, path)
	} else {
		log.Printf("[worktree] removed %s", path)
	}
	return nil
}

// Cleanup removes all orphaned worktrees on startup and prunes bare repos.
// Also sweeps ~/.claude/projects ghost entries for each orphaned cwd.
func Cleanup() {
	CleanupWithOptions(CleanupOptions{})
}

// CleanupOptions controls Cleanup behavior.
type CleanupOptions struct {
	// PreserveClaudeProjectFor names runIDs whose ~/.claude/projects entry
	// must NOT be deleted because their session JSONL is still required
	// for an interactive resume (taken_over runs). The orphaned worktree
	// directory under $TMPDIR is still removed — it's the project dir
	// holding the conversation state that needs to survive.
	//
	// Keys are run IDs. Both wt-style (<runID>) and no-cwd (<runID>-nocwd)
	// directory names are matched. Ignored when SkipClaudeProjectCleanup
	// is true.
	PreserveClaudeProjectFor map[string]bool

	// SkipClaudeProjectCleanup turns OFF deletion of every
	// ~/.claude/projects entry, regardless of the preserve set. Used at
	// startup when the caller can't reliably determine the preserve set
	// (e.g., the DB query for taken-over run IDs failed): rather than
	// risk wiping a session JSONL we should have kept, skip ALL project-
	// dir cleanup for this sweep. The worktree dir removal and bare-repo
	// pruning still run, so we don't leak large temp directories.
	SkipClaudeProjectCleanup bool
}

// CleanupWithOptions is the parameterized Cleanup the takeover flow uses
// at startup to keep session JSONLs alive across crashes — see
// CleanupOptions.PreserveClaudeProjectFor.
func CleanupWithOptions(opts CleanupOptions) {
	// Orphaned-worktree (temp-dir) cleanup. A missing runs dir just means
	// there's nothing to remove here — it must NOT skip the bare-repo
	// sweep below. /tmp is routinely wiped on reboot, which is exactly the
	// case where a locked=initializing ghost survives in the persistent
	// ~/.triagefactory/repos bare and needs reclaiming.
	runsBase := filepath.Join(os.TempDir(), runsDir)
	if entries, err := os.ReadDir(runsBase); err == nil {
		count := 0
		for _, e := range entries {
			if e.IsDir() {
				fullPath := filepath.Join(runsBase, e.Name())
				runID := strings.TrimSuffix(e.Name(), "-nocwd")
				// Project-dir deletion has two opt-outs: a global SkipAll
				// (caller couldn't determine the preserve set) and a per-
				// runID preserve (this run is in taken_over state). Either
				// keeps the JSONL intact while we still remove the worktree
				// dir below.
				if !opts.SkipClaudeProjectCleanup && !opts.PreserveClaudeProjectFor[runID] {
					// Each entry here was a live claude cwd at some point — nuke its
					// ghost ~/.claude/projects entry before removing the dir itself
					// (EvalSymlinks needs the dir to still exist to resolve).
					RemoveClaudeProjectDir(fullPath)
				}
				os.RemoveAll(fullPath)
				count++
			}
		}
		if count > 0 {
			log.Printf("[worktree] cleaned up %d orphaned worktrees", count)
		}
	}

	// Prune all bare repos — always, regardless of the runs dir above.
	// Clear stale locked ghosts first (plain prune skips their lock
	// marker), then run the ordinary prune for unlocked stale entries.
	// Safe to force here: this runs at startup before any poller/spawner
	// can issue a concurrent add.
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	clearStaleLockedWorktreesAll(filepath.Join(home, reposDir))
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

// removeWorktreeRegFor deletes the single bare admin registration that
// belongs to wtDir, if present. A cancelled/killed `git worktree add`
// leaves <bare>/worktrees/<name> behind, locked with git's transient
// "initializing" marker; plain `git worktree prune` skips locked entries
// so the branch stays pinned. git names that admin dir after the
// basename of the worktree path, and our worktree paths are
// runDir(runID) (basename == runID, globally unique), so the entry is
// deterministically worktrees/<basename(wtDir)>.
//
// Targeting by path — rather than sweeping every locked=initializing
// entry in the bare — is what makes this safe to call without the
// per-repo lock: it can only ever touch this run's own dead add, never a
// concurrent add against the same bare (e.g. CopyForTakeover's overlay
// add, whose dir is run-<runID>, doesn't take lockRepo). os.RemoveAll is
// a no-op when the entry was never created (add failed before mkdir).
func removeWorktreeRegFor(bareDir, wtDir string) {
	adminDir := filepath.Join(bareDir, "worktrees", filepath.Base(wtDir))
	if err := os.RemoveAll(adminDir); err != nil {
		log.Printf("[worktree] clear half-built worktree %s: %v", adminDir, err)
		return
	}
}

// worktreeCheckoutExists reports whether the working tree referenced by a
// bare admin entry still exists on disk. It reads the non-localized
// `gitdir` file (the path to <worktree>/.git that git records for every
// linked worktree); a missing/unreadable gitdir or a missing target both
// mean the working tree is gone. Deliberately avoids the `locked` file's
// reason text, which git writes through gettext (_("initializing")) and
// is therefore localized.
func worktreeCheckoutExists(adminDir string) bool {
	data, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		return false
	}
	gitPath := strings.TrimSpace(string(data))
	if gitPath == "" {
		return false
	}
	_, err = os.Stat(gitPath)
	return err == nil
}

// clearStaleLockedWorktrees force-removes admin registrations under
// <bareDir>/worktrees/* that are locked AND whose working tree no longer
// exists. That pair is precisely the set `git worktree prune` refuses to
// reclaim (it skips locked entries) but which is safe to drop: a
// genuinely-in-use locked worktree still has its checkout on disk and is
// left untouched. Returns the number of ghosts cleared.
//
// We key on lock-file presence + missing checkout rather than the lock
// reason string because git localizes that reason via gettext, so the
// literal "initializing" isn't a stable token across locales.
//
// CONCURRENCY: startup-only. An in-progress `git worktree add` is briefly
// locked with its checkout not yet populated, so a concurrent sweep could
// race it — but at startup no poller/spawner/takeover has begun. The
// in-process reclaim (removeWorktreeRegFor) is the lock-free, path-scoped
// counterpart used during normal operation.
func clearStaleLockedWorktrees(bareDir string) int {
	worktreesDir := filepath.Join(bareDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		return 0 // no worktrees admin dir → nothing registered
	}
	cleared := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		adminDir := filepath.Join(worktreesDir, e.Name())
		if _, err := os.Stat(filepath.Join(adminDir, "locked")); err != nil {
			continue // unlocked → plain prune reclaims it if stale
		}
		if worktreeCheckoutExists(adminDir) {
			continue // locked and live → a real worktree, never touch
		}
		if err := os.RemoveAll(adminDir); err != nil {
			log.Printf("[worktree] clear stale locked worktree %s: %v", adminDir, err)
			continue
		}
		log.Printf("[worktree] cleared stale locked worktree registration %s", e.Name())
		cleared++
	}
	return cleared
}

// clearStaleLockedWorktreesAll sweeps every bare repo under baseDir for
// stale locked worktree ghosts and force-removes them. Startup-only — see
// clearStaleLockedWorktrees for the concurrency contract. This is the
// backstop for ghosts left by ungraceful death (SIGKILL, crash, OOM,
// power loss) where no in-process cleanup ran.
func clearStaleLockedWorktreesAll(baseDir string) {
	total := 0
	if err := filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasSuffix(path, ".git") {
			total += clearStaleLockedWorktrees(path)
			return filepath.SkipDir
		}
		return nil
	}); err != nil {
		log.Printf("[worktree] walk %s: %v", baseDir, err)
	}
	if total > 0 {
		log.Printf("[worktree] cleared %d stale locked worktree registration(s)", total)
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
