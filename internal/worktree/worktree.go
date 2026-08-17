package worktree

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// claudeProjectsDir and claudeHome live in claude_session.go alongside the
// rest of the Claude Code session/project-dir handling.

// Per-repo mutexes prevent concurrent fetches from racing on the same bare repo.
var (
	repoMu    sync.Mutex
	repoLocks = map[string]*sync.Mutex{}
)

// onCloneResult is invoked by EnsureBareClone after every clone attempt
// (success or failure) for the per-repo callback wired in main.go. The
// callback writes to repositories.clone_status / clone_error /
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
			worktreeLog.Error("onCloneResult callback panicked", "owner", owner, "repo", repo, "panic", r)
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

// runsDir is the basename for ephemeral run worktrees under
// os.TempDir() (/tmp/triagefactory-runs/{run-id}). Ephemeral and
// unique-by-conversationID, so it is deliberately NOT routed through
// internal/paths — there is no persistence and no cross-tenant
// collision risk. The persistent bare clone cache, by contrast, lives
// under paths.BareCacheDir / paths.BareCacheRoot.
const runsDir = "triagefactory-runs" // worktrees: /tmp/triagefactory-runs/{run-id}

// CloneAuth is an optional HTTPS credential for a host-side `git clone` /
// `git fetch`. The zero value injects nothing — the git subprocess
// runs exactly as before (anonymous HTTPS for public repos, or SSH via the
// operator's agent in local mode). When populated, worktree attaches the
// credential as a host-scoped `http.<prefix>.extraHeader` passed through the
// subprocess *environment* (never argv, never the persisted origin URL), for
// the clone and the fetch in the same call.
//
// This package stays credential-agnostic by design: it doesn't know whether
// the token is a GitHub App installation token or a PAT, only that it's the
// bearer half of GitHub's `x-access-token:<token>` HTTPS Basic credential.
// Resolver-aware callers (the spawner, the curator) mint it; this leaf
// package just attaches it. Build a value with CloneAuthFor.
type CloneAuth struct {
	// urlPrefix is the `scheme://host[:port]/` the extraHeader is scoped to,
	// e.g. "https://github.com/". Host-scoping (rather than a bare
	// http.extraHeader) keeps the token from leaking on a cross-host
	// redirect. Empty disables injection.
	urlPrefix string
	// token is the bearer half of the credential, sent as
	// base64("x-access-token:" + token). Empty disables injection.
	token string

	// The three proxy* fields, set together by CloneAuthViaGitProxy, are the
	// alternative to the real-token path above: instead of injecting a
	// credential, they route this call's network git through the per-run git
	// proxy, which holds the real token. Used on the executor path (TFAC-631)
	// where the clone must not put the org's GitHub token in this process.
	// Fixed string fields (not a slice) so CloneAuth stays comparable — callers
	// assert equality with `!=`.
	//
	// proxyBase scopes both git-config keys; proxyInsteadOf is the upstream
	// prefix rewritten to it (url.<proxyBase>.insteadOf); proxyExtraHeader is
	// the placeholder Basic credential the proxy authenticates
	// (http.<proxyBase>.extraHeader). All empty on the token path.
	proxyBase        string
	proxyInsteadOf   string
	proxyExtraHeader string
}

// CloneAuthFor scopes token to the host of cloneURL for per-invocation
// injection. Returns the zero CloneAuth (no injection) when token is empty or
// cloneURL is not an https:// URL — so SSH remotes (local-mode default) and
// the no-token public path stay byte-for-byte unchanged. The injection is
// thus implicitly HTTPS-only without the caller having to branch on protocol.
func CloneAuthFor(cloneURL, token string) CloneAuth {
	if token == "" || cloneURL == "" {
		return CloneAuth{}
	}
	u, err := url.Parse(cloneURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return CloneAuth{}
	}
	return CloneAuth{urlPrefix: "https://" + u.Host + "/", token: token}
}

// gitProxyPlaceholderUser mirrors agentproc.gitProxyBasicUser ("x-run"): the
// username half of the per-run git Basic credential the proxy-routing mode
// presents. The git proxy validates only the password (the placeholder token),
// so the username is a stable sentinel — kept identical to the sandbox agent's
// own git-proxy user so host clone and in-jail agent transit the proxy the same
// way.
const gitProxyPlaceholderUser = "x-run"

// CloneAuthViaGitProxy routes a host-side clone/fetch through the per-run git
// proxy instead of injecting a real token. Every network git op is rewritten
// from upstream to proxyURL (git url.insteadOf) and carries placeholderToken as
// its Basic password (http.extraHeader); the proxy — the credential sidecar —
// swaps the placeholder for the org's real GitHub token on the upstream hop, so
// THIS process never holds it (TFAC-631). The encoding matches the sandbox
// agent's own git-proxy pairs so the host-side clone and the in-jail agent
// transit the one proxy identically. Returns the zero (inert) CloneAuth if any
// argument is empty.
func CloneAuthViaGitProxy(proxyURL, upstream, placeholderToken string) CloneAuth {
	if proxyURL == "" || upstream == "" || placeholderToken == "" {
		return CloneAuth{}
	}
	creds := gitProxyPlaceholderUser + ":" + placeholderToken
	return CloneAuth{
		proxyBase:        strings.TrimRight(proxyURL, "/") + "/",
		proxyInsteadOf:   strings.TrimRight(upstream, "/") + "/",
		proxyExtraHeader: "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(creds)),
	}
}

// active reports whether this CloneAuth will inject anything — either the
// real-token extraHeader or the git-proxy routing pair.
func (a CloneAuth) active() bool {
	return (a.urlPrefix != "" && a.token != "") || a.proxyBase != ""
}

// configEntry is the single git config (key, value) for the real-token mode
// (CloneAuthFor): a host-scoped HTTP extraHeader whose value mirrors the
// gitproxy's Basic-auth encoding, base64 of "x-access-token:" + token. ok=false
// for the zero value AND for the proxy-routing mode (which emits two entries —
// see configEntries). Split out so gitConfigEnviron and the token-mode test
// share one source of the encoding.
func (a CloneAuth) configEntry() (key, value string, ok bool) {
	if a.urlPrefix == "" || a.token == "" {
		return "", "", false
	}
	creds := "x-access-token:" + a.token
	return "http." + a.urlPrefix + ".extraHeader",
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(creds)),
		true
}

// configEntries returns every git-config (key, value) this auth injects: the
// single host-scoped extraHeader in the token mode, or the insteadOf +
// placeholder-extraHeader pair in the proxy-routing mode. Empty when inert.
func (a CloneAuth) configEntries() [][2]string {
	if key, value, ok := a.configEntry(); ok {
		return [][2]string{{key, value}}
	}
	if a.proxyBase != "" {
		return [][2]string{
			{"url." + a.proxyBase + ".insteadOf", a.proxyInsteadOf},
			{"http." + a.proxyBase + ".extraHeader", a.proxyExtraHeader},
		}
	}
	return nil
}

// GitConfigEntries is the exported view of configEntries: the exact git-config
// (key, value) pairs this auth puts in front of a network git invocation, in
// order. Same rationale as CloneAuthFromOptions — a caller that threads a
// credential asserts what its git would actually run under, rather than that it
// passed some opaque value. Empty for the inert zero CloneAuth.
func (a CloneAuth) GitConfigEntries() [][2]string {
	return a.configEntries()
}

// gitConfigEnviron returns base (a parent environment, e.g. os.Environ())
// extended with this auth's extraHeader as a git env-config entry, plus true;
// or (nil, false) when the auth is inert, so the caller leaves cmd.Env unset
// and the subprocess inherits the parent env unchanged.
//
// Git's env-config form (GIT_CONFIG_COUNT + GIT_CONFIG_KEY_n / _VALUE_n) is
// used instead of a `-c key=value` argv flag so the token never appears in the
// subprocess argv (which any host process can read via ps or /proc); a child's
// environment is far less exposed.
//
// The entry is appended at the next free index — one past the inherited
// GIT_CONFIG_COUNT (0 when unset) — rather than always at index 0, so a
// pre-existing operator-provided GIT_CONFIG_* set (e.g. a custom CA) is
// preserved and our header composes alongside it instead of clobbering it. The
// inherited COUNT line is dropped and re-emitted bumped, so git reads a single,
// correct COUNT and our entry regardless of how duplicate env keys would
// otherwise resolve — we never depend on first-vs-last-wins dedup behavior.
func (a CloneAuth) gitConfigEnviron(base []string) ([]string, bool) {
	entries := a.configEntries()
	if len(entries) == 0 {
		return nil, false
	}
	idx := gitConfigCount(base)
	out := make([]string, 0, len(base)+1+2*len(entries))
	for _, kv := range base {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			continue // re-emitted below, bumped to include our entries
		}
		out = append(out, kv)
	}
	out = append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(idx+len(entries)))
	for i, e := range entries {
		n := idx + i
		out = append(out,
			"GIT_CONFIG_KEY_"+strconv.Itoa(n)+"="+e[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"="+e[1],
		)
	}
	return out, true
}

// gitConfigCount parses GIT_CONFIG_COUNT (the number of git's indexed
// env-config entries) from env, or 0 when it is absent, malformed, or
// negative. Scans last-to-first so a duplicated COUNT resolves the way git and
// Go's exec env-dedup would (last value wins).
func gitConfigCount(env []string) int {
	for i := len(env) - 1; i >= 0; i-- {
		v, ok := strings.CutPrefix(env[i], "GIT_CONFIG_COUNT=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			return n
		}
		return 0
	}
	return 0
}

// CloneOption configures an optional aspect of a worktree clone/fetch entry
// point. Variadic options keep the ~30 existing call sites (mostly tests)
// compiling unchanged while letting resolver-aware callers thread a
// credential where they have one.
type CloneOption func(*cloneConfig)

type cloneConfig struct {
	auth       CloneAuth
	seedURL    string
	baseBranch string
}

// WithCloneAuth attaches an HTTPS credential to the host-side git clone +
// fetch for this call. A zero CloneAuth (e.g. from CloneAuthFor on an SSH URL
// or empty token) is a no-op, so callers can pass it unconditionally.
func WithCloneAuth(auth CloneAuth) CloneOption {
	return func(c *cloneConfig) { c.auth = auth }
}

// WithCloneURL supplies the upstream clone URL an entry point may use to
// seed a missing bare on demand. EnsureCuratorWorktree consumes it to
// retire its old refuse-to-seed behavior (TFAC-60/-62): a curator dispatch
// on a pinned repo that was never delegated against now seeds the bare
// itself via the same idempotent path delegation uses, rather than erroring
// with the misleading "repo profiling has not run yet". Empty is a no-op —
// the entry point falls back to whatever its no-URL behavior was.
//
// Only EnsureCuratorWorktree honors this option. EnsureBareClone,
// CreateForPR, and CreateForBranch* already take the clone URL as an
// explicit positional parameter, so passing WithCloneURL to them is a
// silent no-op — use the positional argument there.
func WithCloneURL(url string) CloneOption {
	return func(c *cloneConfig) { c.seedURL = url }
}

// WithBaseBranch names the PR's base branch (e.g. "main") so CreateForPR /
// CreateForPRInRoot refresh its remote-tracking ref (refs/remotes/origin/<base>)
// alongside the PR head at materialization. Only those two honor it; the PR diff
// (cmd/exec/gh) frames against this ref, and the per-run PR fetch otherwise only
// touches the head, leaving a clone-time-frozen base ref that misframes the diff
// (TFAC-505). Empty is a no-op — no base refresh, and the diff path falls back to
// the recorded base.sha / API. Other entry points ignore it.
func WithBaseBranch(branch string) CloneOption {
	return func(c *cloneConfig) { c.baseBranch = branch }
}

func resolveCloneOptions(opts []CloneOption) cloneConfig {
	var c cloneConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	return c
}

// BaseBranchFromOptions resolves the base branch carried by WithBaseBranch in a
// CloneOption set, or "" when none was supplied. Exposed so callers and tests
// can assert which base branch they wired without reaching into the unexported
// cloneConfig — e.g. the workspace-add path verifying it passes WithBaseBranch(pr.BaseRef).
func BaseBranchFromOptions(opts ...CloneOption) string {
	return resolveCloneOptions(opts).baseBranch
}

// CloneAuthFromOptions resolves the CloneAuth carried by WithCloneAuth in a
// CloneOption set (the zero CloneAuth when none was supplied). Same rationale
// as BaseBranchFromOptions: callers' tests assert which credential they wired
// by comparing against CloneAuthFor's output — CloneAuth is a comparable value
// — without reaching into the unexported cloneConfig.
func CloneAuthFromOptions(opts ...CloneOption) CloneAuth {
	return resolveCloneOptions(opts).auth
}

func repoDir(owner, repo string) (string, error) {
	// orgID is the local-default sentinel for now; threading the
	// real orgID through here would make this bare cache bounded + evictable.
	// StateRootErr surfaces a missing $HOME the way the pre-paths
	// os.UserHomeDir call did, rather than letting BareCacheDir panic.
	if _, err := paths.StateRootErr(); err != nil {
		return "", err
	}
	return paths.BareCacheDir(runmode.LocalDefaultOrgID, owner, repo), nil
}

// RepoDir is the exported variant of repoDir for callers outside the worktree
// package that need the bare clone's path to run git against it directly (e.g.
// the workspace-snapshot tests). Wrapper rather than renaming repoDir to keep
// the existing internal call sites untouched.
func RepoDir(owner, repo string) (string, error) {
	return repoDir(owner, repo)
}

// rootKey names the id a run root is keyed by — see RunRoot for which id that
// is, and why this family does not spell it conversationID.
func runDir(rootKey string) string {
	return filepath.Join(os.TempDir(), runsDir, rootKey)
}

// RunRoot returns the run-root path for a given rootKey, without
// creating it. The Jira lazy-worktree CLI calls this from a delegated
// agent process to derive the parent directory under which `workspace
// add` materializes per-repo worktrees as `{runRoot}/{owner}/{repo}/`.
// Callers who need the directory to exist on disk should use MakeRunRoot
// instead.
//
// rootKey is a key, not a conversation id, and this family takes no view on
// which id a caller keys its tree by — it only requires that the caller uses
// the same one for the make, the derive, and the remove. The orchestrator keys
// every delegated run's tree by the blueprint run id (the memory namespace), so
// one blueprint's steps share a root; the in-jail agent host derives the same
// path from its conversation id only as the fallback for a run whose recorded
// worktree_path is missing, which is exactly where the two diverge (see
// cmd/exec/agenthost's WorkspaceRoots).
func RunRoot(rootKey string) string {
	return runDir(rootKey)
}

// MakeRunRoot creates the run-root directory and returns its absolute
// path. Used by the spawner's setupJira path: the agent's initial cwd
// is the run-root (a throwaway dir holding only the _tfac/ scratch
// subdirs until the agent calls `workspace add` to materialize
// worktrees as subdirs).
//
// Single-purpose vs. CreateForBranch: CreateForBranch creates a worktree
// AT runDir(rootKey); MakeRunRoot creates only the directory itself, with
// no git contents. The Jira lazy path uses MakeRunRoot so the agent has
// somewhere to land before it has chosen which repo(s) to materialize.
func MakeRunRoot(rootKey string) (string, error) {
	dir := runDir(rootKey)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir run root: %w", err)
	}
	// The run root is the agent's cwd AND its in-jail HOME, so a blueprint step
	// running on a Jira task discovers its skill through the same symlink a
	// GitHub PR run's worktree carries.
	plantSandboxSkillsLink(dir)
	return dir, nil
}

// RemoveRunRoot removes the run-root directory and everything under it
// (including any per-repo worktrees that were materialized as subdirs).
// Safe if missing. Cleanup of the bare-side worktree registrations is
// handled by RemoveAt + pruneAll for each individual worktree before
// this is called; this is the final sweep of the parent dir itself.
func RemoveRunRoot(rootKey string) {
	// Privileged seam (see RemoveAt's doc): the run root is sandbox-owned
	// by teardown time in multi mode.
	_ = sandbox.RemoveRunTree(context.Background(), runDir(rootKey))
}

// MakeRunCwd creates a throwaway cwd for delegated runs that have no worktree.
// Vestigial — superseded by MakeRunRoot now that Jira runs always populate
// the run-root and materialize worktrees as subdirs. Kept temporarily so
// future cleanup callers don't break; remove in a follow-up once no callers
// remain.
func MakeRunCwd(conversationID string) (string, error) {
	dir := filepath.Join(os.TempDir(), runsDir, conversationID+"-nocwd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("mkdir run cwd: %w", err)
	}
	return dir, nil
}

// RemoveRunCwd removes the throwaway cwd created by MakeRunCwd. Safe if missing.
// Vestigial — see MakeRunCwd.
func RemoveRunCwd(conversationID string) {
	_ = sandbox.RemoveRunTree(context.Background(), filepath.Join(os.TempDir(), runsDir, conversationID+"-nocwd"))
}

// EnsureBareClone is the exported entry point for callers that want a
// bare clone of owner/repo materialized. It's idempotent: if the bare
// already exists, it only repairs a drifted origin URL; otherwise it
// clones the bare and then repairs origin if needed. Bootstrap calls
// this for every configured repo on startup so first-delegation
// latency disappears.
//
// The cloneURL must be the upstream repository's URL (the URL stored
// in repositories.clone_url, populated during repo profiling). Passing
// a fork's URL would clobber the bare's origin and is the historical
// bug this function exists to prevent — see repairOriginURL.
func EnsureBareClone(ctx context.Context, owner, repo, cloneURL string, opts ...CloneOption) (string, error) {
	mu := lockRepo(owner, repo)
	mu.Lock()
	defer mu.Unlock()
	return ensureBareCloneLocked(ctx, owner, repo, cloneURL, resolveCloneOptions(opts).auth)
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
func ensureBareCloneLocked(ctx context.Context, owner, repo, cloneURL string, auth CloneAuth) (bareDir string, err error) {
	// Fire the post-clone callback exactly once per call so consumers
	// (main.go's hook → repositories + websocket) see one event per
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
			worktreeLog.Error("ensureBareClone failed", "owner", owner, "repo", repo, "error", err)
		}
		go fireCloneResult(owner, repo, err)
	}()

	bareDir, err = cloneBareIfMissing(ctx, owner, repo, cloneURL, auth)
	if err != nil {
		return "", err
	}
	if cloneURL != "" {
		// repairOriginURL writes the PLAIN URL (never the token) — the
		// credential lives only in the clone/fetch subprocess env, so the
		// persisted remote.origin.url stays clean on disk.
		if err = repairOriginURL(ctx, bareDir, cloneURL); err != nil {
			return "", fmt.Errorf("repair origin url: %w", err)
		}
	}
	// Account the access for the bounded-cache LRU (cache.go). Stamping on
	// every seed/refresh keeps the reaper's coldest-first ordering honest;
	// in local (unbounded policy) the stamp is harmless bookkeeping.
	touchBare(bareDir)
	return bareDir, nil
}

// cloneBareIfMissing performs the actual `git clone --bare` only when
// the bare directory doesn't yet exist. Caller must hold the per-repo
// lock. Does NOT configure origin URL or refspecs — see
// ensureBareCloneLocked for the full lifecycle.
func cloneBareIfMissing(ctx context.Context, owner, repo, cloneURL string, auth CloneAuth) (string, error) {
	bareDir, err := repoDir(owner, repo)
	if err != nil {
		return "", fmt.Errorf("resolve repo dir: %w", err)
	}

	if _, err := os.Stat(bareDir); os.IsNotExist(err) {
		if cloneURL == "" {
			return "", fmt.Errorf("bare clone for %s/%s missing and no cloneURL provided", owner, repo)
		}
		worktreeLog.Info("cloning (first time)", "owner", owner, "repo", repo)
		if err := os.MkdirAll(filepath.Dir(bareDir), 0755); err != nil {
			return "", fmt.Errorf("mkdir: %w", err)
		}
		start := time.Now()
		if err := gitRunCtxAuth(ctx, "", auth, "clone", "--bare", "--filter=blob:none", cloneURL, bareDir); err != nil {
			return "", fmt.Errorf("bare clone: %w", err)
		}
		worktreeLog.Debug("clone completed", "owner", owner, "repo", repo, "duration", time.Since(start).Round(time.Millisecond))
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
	worktreeLog.Debug("repairing origin url", "dir", bareDir, "current", currentURL, "want", wantURL)
	return gitRunCtx(ctx, bareDir, "remote", "set-url", "origin", wantURL)
}

// makeWorktreeDir creates the run directory for a worktree.
func makeWorktreeDir(conversationID string) (string, error) {
	wtDir := runDir(conversationID)
	if err := os.MkdirAll(filepath.Dir(wtDir), 0755); err != nil {
		return "", fmt.Errorf("mkdir runs: %w", err)
	}
	return wtDir, nil
}

// gitBaseEnv is the parent environment every git subprocess in this package
// runs with: the process environment with GIT_TERMINAL_PROMPT forced to 0.
// Disabling the prompt is essential on a headless server — git can never
// satisfy an interactive "Username for 'https://...'" prompt, so without this a
// missing or invalid credential would block on (or fail opaquely against)
// /dev/tty instead of returning the clear "terminal prompts disabled" error
// fast. Crucially, the setting is inherited by any child git process too —
// including the lazy promisor fetch git spawns inside `worktree add` / `reset
// --hard` on a blobless bare — so a deferred-blob fetch that can't authenticate
// fails fast as well.
//
// Any inherited GIT_TERMINAL_PROMPT is dropped before the =0 is appended, so a
// parent process that preset =1 can't re-enable interactive prompts — the
// guarantee holds regardless of how the exec layer resolves duplicate env keys.
//
// GIT_ASKPASS / SSH_ASKPASS are stripped for the same reason, and they matter
// just as much: an editor-managed terminal (Cursor, VS Code) exports GIT_ASKPASS
// pointing at a GUI credential helper, and git invokes it REGARDLESS of
// GIT_TERMINAL_PROMPT=0 — so an unauthenticated fetch pops a graphical
// "Username for 'https://...'" prompt and hangs forever instead of failing fast.
// TF always injects credentials via the auth header / git proxy and never wants
// an interactive prompt, so the inherited askpass helpers are dropped from every
// child env. (Headless servers have neither set, so this is inert there; it's
// the local-dev path — and its test suite — that would otherwise hang.)
func gitBaseEnv() []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") ||
			strings.HasPrefix(kv, "GIT_ASKPASS=") ||
			strings.HasPrefix(kv, "SSH_ASKPASS=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

// TrackedUnder returns the slash-separated repo-relative paths git tracks under
// prefix in the working tree at dir. Empty for a prefix the repo knows nothing
// about, and for a dir that is no working tree at all (a Jira run-root) — a
// caller uses this to tell the repo's files from ours, and "not a repo" means
// every path under the prefix can only be ours.
//
// This is what makes .git/info/exclude's blind spot safe to work around: an
// exclude pattern does nothing for an already-tracked path, so a repo that
// happens to track something under the directory TF claims would have that
// content mutated by an infrastructure write and the change swept into the
// agent's next commit. Ask git, don't infer from the exclude list.
//
// Errors are folded into "nothing tracked" deliberately: the answer's only use
// is to hold TF back from touching a path, and a git that won't answer is not a
// reason to fail a run.
// AdoptLegacyScratchDir renames a run tree's pre-rename scratch dir to the
// current name, so a run reusing a tree an older binary built still finds the
// files an earlier step of the same workflow dropped there — a review pass's
// findings, a downloaded CI log — where this binary's prompts say they are.
// Without it that step reads an empty directory and, per its own instructions,
// proceeds as though there was nothing to read.
//
// Refuses to move anything the repo tracks (the dir would be the repo's, not
// ours) and never overwrites an existing current-name dir. Best-effort
// otherwise: on failure the tree keeps both names and the run proceeds.
func AdoptLegacyScratchDir(ctx context.Context, dir string) {
	if dir == "" {
		return
	}
	legacy := filepath.Join(dir, legacyScratchDir)
	if info, err := os.Stat(legacy); err != nil || !info.IsDir() {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, ScratchDir)); err == nil {
		return
	}
	if len(TrackedUnder(ctx, dir, legacyScratchDir)) > 0 {
		worktreeLog.Warn("repo tracks files under the legacy scratch dir; leaving it in place", "dir", legacy)
		return
	}
	if err := os.Rename(legacy, filepath.Join(dir, ScratchDir)); err != nil {
		worktreeLog.Warn("adopt legacy scratch dir failed", "dir", legacy, "error", err)
	}
}

func TrackedUnder(ctx context.Context, dir, prefix string) map[string]bool {
	if dir == "" || prefix == "" {
		return nil
	}
	out, err := gitOutputCtx(ctx, dir, "ls-files", "-z", "--", prefix)
	if err != nil {
		return nil
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	return tracked
}

func gitOutputCtx(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitBaseEnv()
	return runGitOutput(ctx, cmd, args)
}

// runGitOutput executes cmd (already configured by the caller) and returns
// its combined output, or a formatted error — the shared tail of gitOutputCtx
// and the config-file reads in worktree_branch.go.
func runGitOutput(ctx context.Context, cmd *exec.Cmd, args []string) (string, error) {
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

func gitRunCtx(ctx context.Context, dir string, args ...string) error {
	return gitRunCtxAuth(ctx, dir, CloneAuth{}, args...)
}

// gitRunCtxAuth runs git, injecting auth's extraHeader into the subprocess
// environment when auth is active (composed at the next free GIT_CONFIG index
// — see gitConfigEnviron). The env is always set, layered on gitBaseEnv() so
// GIT_TERMINAL_PROMPT=0 reaches the subprocess (and the lazy promisor fetch it
// spawns) whether or not auth is active; an inert auth uses that base alone,
// behaving identically to before aside from the now-explicit prompt disable.
func gitRunCtxAuth(ctx context.Context, dir string, auth CloneAuth, args ...string) error {
	for attempt := 1; ; attempt++ {
		cmd := exec.CommandContext(ctx, "git", args...)
		if dir != "" {
			cmd.Dir = dir
		}
		base := gitBaseEnv()
		if env, ok := auth.gitConfigEnviron(base); ok {
			cmd.Env = env
		} else {
			cmd.Env = base
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled")
		}
		runErr := fmt.Errorf("%s: %s", err, string(out))
		if attempt >= worktreeAddLockRaceMaxAttempts || !isWorktreeAddLockRace(args, out) {
			return runErr
		}
		// `worktree add` writes its own transient worktrees/<id>/locked
		// marker (to keep a concurrent `worktree prune` off the entry while
		// checkout populates) immediately after mkdir-ing worktrees/<id>/ —
		// two syscalls apart, no window for OUR code to interleave. Under
		// heavy parallel disk I/O this has still been observed to fail with
		// "could not open '...locked' for writing: No such file or
		// directory", i.e. git's own mkdir-then-open raced with something
		// outside our control. Nothing was registered under this id (the
		// mkdir that would have claimed it didn't survive), so a retry is
		// safe — it just repeats the same not-yet-started add.
		worktreeLog.Warn("worktree add hit transient locked-marker race, retrying", "dir", dir, "attempt", attempt, "error", runErr)
		// ctx-aware wait: a plain time.Sleep would keep a cancelled/timed-out
		// caller blocked for the full backoff instead of returning promptly.
		select {
		case <-ctx.Done():
			return fmt.Errorf("cancelled")
		case <-time.After(worktreeAddLockRaceBackoff):
		}
	}
}

// worktreeAddLockRaceMaxAttempts bounds the retry in gitRunCtxAuth for the
// transient `worktree add` locked-marker race documented there.
const worktreeAddLockRaceMaxAttempts = 3

// worktreeAddLockRaceBackoff is the pause between retries — long enough to
// let whatever transient condition caused the race clear, short enough that
// three attempts add negligible latency to the common (non-racing) path. A
// var, not a const, so tests can lengthen it to deterministically exercise
// the ctx-cancellation-during-backoff path without a real multi-second wait.
var worktreeAddLockRaceBackoff = 50 * time.Millisecond

// worktreeAddLockRaceRe matches git's exact locked-marker failure text —
// "could not open '<path ending in /locked>' for writing: No such file or
// directory" — as ONE contiguous phrase, not two independent substrings.
// Requiring contiguity (rather than separately checking for "locked' for
// writing" and "No such file or directory" anywhere in the output) rules out
// an unrelated `worktree add` failure that happens to mention some other
// locked file in one sentence and a missing directory in another.
var worktreeAddLockRaceRe = regexp.MustCompile(`could not open '[^']*locked' for writing: No such file or directory`)

// isWorktreeAddLockRace reports whether args/out match the narrow
// `worktree add` failure signature described in gitRunCtxAuth: git's own
// transient locked-marker write failing with a missing-directory error.
// Matched narrowly (both the subcommand and the exact error shape) so this
// never masks a real `worktree add` failure — a bad ref, an already
// registered worktree, an auth failure — which all produce different text.
func isWorktreeAddLockRace(args []string, out []byte) bool {
	if len(args) < 2 || args[0] != "worktree" || args[1] != "add" {
		return false
	}
	return worktreeAddLockRaceRe.Match(out)
}

func gitRun(dir string, args ...string) error {
	return gitRunCtx(context.Background(), dir, args...)
}
