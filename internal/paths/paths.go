// Package paths is the single source of truth for every persistent
// on-disk path Triage Factory owns. It is mode- and org-aware: local
// mode lays state out byte-for-byte the way the pre-multi binary did
// (~/.triagefactory/...), while multi mode inserts an org segment for
// the tenant-scoped subtrees so two orgs sharing a pod never collide.
//
// Three path classes, deliberately NOT collapsed into one flat swap —
// the distinction is what keeps tenant isolation and the shared sandbox
// rootfs both correct:
//
//   - Class 1, org-scoped persistent — the bare clone cache. This gains an
//     /orgs/<orgID>/ segment in multi mode so each tenant's clones live
//     under their own subtree. Resolved through OrgRoot.
//   - Class 2, host-global persistent — the sandbox rootfs cache and the
//     Agent SDK install. Shared read-only across every tenant by design;
//     org-scoping them would re-multiply identical toolchains per org
//     and defeat page-cache sharing, so they hang off ToolchainRoot (the
//     runtime image pins it via TF_TOOLCHAIN_ROOT to the fixed bake path;
//     it otherwise falls back to StateRoot), never OrgRoot.
//   - Class 3, local-only — the SQLite DB. Multi mode uses Postgres, so
//     this hangs off StateRoot with no org segment.
//
// os.UserHomeDir and the ".triagefactory" path literal live ONLY in
// this package; a forbidigo rule (.golangci.yml) plus a lint.sh grep
// guard keep every other package routing through these resolvers. The
// one documented exception is ~/.claude/projects — the Claude Code SDK's
// own session-cache directory (an SDK implementation detail, unrelated to
// TF's own vocabulary), which follows the HOME of the process that RAN
// the agent. For direct runs (local mode, or multi on non-Linux) that is
// this process's real HOME; for sandboxed runs (multi + Linux) the agent
// executes with HOME=/work and its session state lands INSIDE the
// run's own org-scoped directory — never the orchestrator's home.
// worktree.ClaudeProjectDir owns that branch (TFAC-109); the nolint'd
// os.UserHomeDir sites under internal/worktree serve only the
// direct-run half.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// dirName is the local-mode state-root basename — the one place the
// ".triagefactory" literal lives. Existing installs key off this exact
// spelling, so it never changes without a migration.
const dirName = ".triagefactory"

// defaultMultiStateRoot is the multi-mode state root when TF_STATE_ROOT
// is unset: a conventional container mount point. Self-host / Fly
// deployments point TF_STATE_ROOT at their mounted volume instead (this
// is the hook shared-nothing executors lean on).
const defaultMultiStateRoot = "/data"

// envStateRoot overrides the multi-mode state-root base.
const envStateRoot = "TF_STATE_ROOT"

// envToolchainRoot overrides the toolchain-root base (the Agent SDK
// install + sandbox rootfs cache). The runtime image sets it so the
// baked toolchain resolves to a fixed path independent of where
// TF_STATE_ROOT points the tenant data. See ToolchainRoot.
const envToolchainRoot = "TF_TOOLCHAIN_ROOT"

// testRoot, when non-empty, overrides StateRoot for the duration of a
// test (set via SetForTest, restored through t.Cleanup). Guarded by
// testRootMu so a parallel suite's writer can't race StateRoot's
// readers. Deliberately separate from runmode's mode state: a test can
// pin where state lands without touching the runtime mode, and a
// mode-flipping test need not also relocate the root.
var (
	testRoot   string
	testRootMu sync.RWMutex
)

// StateRoot returns the base directory under which all persistent TF
// state lives. It is the error-free convenience form of StateRootErr,
// panicking on the (in practice unreachable) case where local mode
// cannot resolve $HOME. Callers whose own signature returns an error
// should use StateRootErr — or pre-flight it once — so a missing home
// surfaces as an actionable error rather than a panic; every swept
// error-returning call site does exactly that.
//
//   - A SetForTest override always wins (tests relocate state onto a
//     t.TempDir() rather than the developer's real home).
//   - Multi mode: TF_STATE_ROOT if set, else defaultMultiStateRoot
//     ("/data"). The env knob is what lets self-host point state at a
//     mounted volume.
//   - Local mode: ~/.triagefactory, always. TF_STATE_ROOT is a
//     multi-mode concern; honoring it locally would put existing
//     installs' "byte-for-byte unchanged" guarantee at the mercy of a
//     stray env var.
func StateRoot() string {
	root, err := StateRootErr()
	if err != nil {
		panic(fmt.Sprintf("paths: %v (is $HOME set?)", err))
	}
	return root
}

// StateRootErr is the error-returning form of StateRoot. It only ever
// errors in local mode when os.UserHomeDir fails (i.e. $HOME unset) —
// the SetForTest / TF_STATE_ROOT overrides and multi mode's defaults
// never touch the home dir. Error-returning callers (db.Open, the
// sandbox/SDK caches, the worktree bare cache, …) route
// through here so a missing home is reported the way it was before the
// internal/paths sweep, rather than panicking.
func StateRootErr() (string, error) {
	testRootMu.RLock()
	tr := testRoot
	testRootMu.RUnlock()
	if tr != "" {
		return tr, nil
	}
	if runmode.Current() == runmode.ModeMulti {
		if env := strings.TrimSpace(os.Getenv(envStateRoot)); env != "" {
			return env, nil
		}
		return defaultMultiStateRoot, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for state root: %w", err)
	}
	return filepath.Join(home, dirName), nil
}

// OrgRoot returns the per-tenant subtree root for org-scoped state.
//
// In multi mode with a real org it is <StateRoot>/orgs/<orgID>. In
// local mode, OR when orgID is the local-default sentinel, the org
// segment is stripped and OrgRoot collapses to StateRoot. That strip is
// what keeps existing local installs byte-for-byte on disk and lets
// callers that aren't org-threaded yet pass runmode.LocalDefaultOrgID
// today without changing the local layout — the real orgID gets
// threaded through the worktree cache afterward.
func OrgRoot(orgID string) string {
	root := StateRoot()
	if runmode.Current() != runmode.ModeMulti || orgID == runmode.LocalDefaultOrgID {
		return root
	}
	return filepath.Join(root, "orgs", orgID)
}

// --- Class 1: org-scoped persistent --------------------------------------

// BareCacheRoot is the parent of every owner/repo bare clone for an
// org: <OrgRoot>/repos. Worktree's prune / stale-lock cleanup sweeps
// walk this whole tree.
func BareCacheRoot(orgID string) string {
	return filepath.Join(OrgRoot(orgID), "repos")
}

// BareCacheDir is the bare clone path for one repo:
// <OrgRoot>/repos/<owner>/<repo>.git. This is consumed while threading
// a real orgID through the bounded, evictable worktree cache.
func BareCacheDir(orgID, owner, repo string) string {
	return filepath.Join(BareCacheRoot(orgID), owner, repo+".git")
}

// --- Class 2: host-global persistent (NEVER org-scoped) ------------------

// ToolchainRoot is the base for image-baked, host-global toolchain
// artifacts — the Agent SDK install and the sandbox rootfs cache. It
// resolves TF_TOOLCHAIN_ROOT if set, else falls back to StateRoot.
//
// The fallback preserves the historical layout: with the env unset
// (every laptop, and host-binary multi-mode dev runs) the toolchain
// lives under the state root exactly as before, so on-disk paths are
// byte-for-byte unchanged and EnsureSDK still installs somewhere
// writable.
//
// The override exists for the one thing the state root can't serve: the
// runtime image bakes the SDK at BUILD time at a fixed path, but
// StateRoot is operator-configurable (TF_STATE_ROOT) and doubles as the
// data-volume mount point. Resolving the baked toolchain through
// StateRoot would let a TF_STATE_ROOT override — or a Fly/k8s volume
// mounted at the data root — shadow or orphan the baked files, and the
// runtime layer has no node/npm to reinstall them. So the image sets
// TF_TOOLCHAIN_ROOT to the fixed bake path (/opt/triagefactory), pinning
// the toolchain there in every mode while TF_STATE_ROOT stays free for
// tenant data.
func ToolchainRoot() string {
	if env := strings.TrimSpace(os.Getenv(envToolchainRoot)); env != "" {
		return env
	}
	return StateRoot()
}

// ToolchainRootErr is the error-returning form of ToolchainRoot, for
// callers that pre-flight a write under the toolchain root (EnsureSDK,
// the rootfs extractor) and want a missing $HOME reported cleanly rather
// than as a panic from the error-free form. It mirrors ToolchainRoot's
// resolution: an explicit TF_TOOLCHAIN_ROOT is a literal path with no
// home dependency (never errors), so the only error path is the StateRoot
// fallback in local mode with $HOME unresolvable.
func ToolchainRootErr() (string, error) {
	if env := strings.TrimSpace(os.Getenv(envToolchainRoot)); env != "" {
		return env, nil
	}
	return StateRootErr()
}

// SandboxRootfsDir is the cached sandbox rootfs for a toolchain cache
// key: <ToolchainRoot>/sandbox/rootfs-<cacheKey>. Mounted read-only and
// shared across all tenants — host-global, never org-scoped (org-scoping
// it would re-extract an identical multi-hundred-MB tree per org and
// defeat page-cache sharing).
func SandboxRootfsDir(cacheKey string) string {
	return filepath.Join(ToolchainRoot(), "sandbox", "rootfs-"+cacheKey)
}

// SDKDir is the Agent SDK install: <ToolchainRoot>/sdk. Identical for
// every tenant, so host-global rather than org-scoped.
func SDKDir() string {
	return filepath.Join(ToolchainRoot(), "sdk")
}

// --- host-global but StateRoot-anchored (NOT ToolchainRoot) --------------
//
// Unlike the Class-2 resolvers above, the hooks dir is written by the
// running binary at startup (not baked into the image), so it must live on
// the writable state volume — hence StateRoot, not the image-baked
// ToolchainRoot. It is still host-global (no org segment): the hooks are
// generic and identical for every tenant.

// HooksDir is the TF-controlled git hooks directory: <StateRoot>/hooks.
// It hosts the hooks installed (process-scoped, via core.hooksPath) into
// every git operation a delegated agent performs — in both run modes and
// any cwd, including repos the agent clones into subdirs itself (F2,
// TFAC-456). The hooks are generic and read run context from env
// (TRIAGE_FACTORY_CONVERSATION_ID + the agenthost socket), so this is a single
// fixed location, NOT regenerated per run — hence host-global and NOT
// org-scoped (it hangs off StateRoot, never OrgRoot). In multi mode the
// host process owns and writes this dir, then bind-mounts it read-only
// into each sandbox; in local mode it lives under ~/.triagefactory and
// the agent subprocess points at it directly.
func HooksDir() string {
	return filepath.Join(StateRoot(), "hooks")
}

// GHBinDir is the directory holding the TF-owned `gh` release binary:
// <StateRoot>/bin. Like HooksDir it is fetched/written by the running binary
// rather than baked into an image, so it hangs off the writable state root and
// NOT the image-baked ToolchainRoot — the runtime image bakes its own gh at a
// fixed path and never consults this one.
//
// The directory itself is the unit, not just the file: it is prepended to the
// agent subprocess's PATH so `gh` resolves to the pinned binary and never to
// whatever gh the user has installed (theirs carries the user's own GitHub
// auth, entirely outside the credential channel). Keep it TF-owned and
// single-purpose for that reason.
func GHBinDir() string {
	return filepath.Join(StateRoot(), "bin")
}

// GHBinaryPath is the TF-owned gh binary: <GHBinDir>/gh.
func GHBinaryPath() string {
	return filepath.Join(GHBinDir(), "gh")
}

// GHChannelDir is one local-mode conversation's private gh-channel directory:
// <StateRoot>/gh-channel/<conversationID>. It holds the engagement's injector
// trust file and gh config dir — created at engagement start and removed when
// the engagement ends.
//
// The config dir matters as much as the cert: local mode runs the agent under
// the user's own HOME, so without an explicit GH_CONFIG_DIR gh would read the
// user's ~/.config/gh — including the hosts entry carrying their personal
// credential. Pointing it at an empty per-conversation dir is the local-mode
// analogue of the jail's HOME isolation in multi mode.
func GHChannelDir(conversationID string) string {
	return filepath.Join(StateRoot(), "gh-channel", sanitizePathSegment(conversationID))
}

// AgentHostSocketDir is the directory holding the per-conversation agenthost
// sockets a LOCAL sandboxed run's exec verbs dial: <StateRoot>/agenthost.
//
// Multi mode has its own root for these (/run/tf, created by the cap-broker
// and validated by it as the only legitimate mount source), which an
// unprivileged local process cannot write. So local gets its own home under
// the state root it already owns — well inside the 108-byte sun_path limit a
// unix socket address is capped at, which is the real constraint on where
// these may live.
func AgentHostSocketDir() string {
	return filepath.Join(StateRoot(), "agenthost")
}

// AgentHostSocketPath is one conversation's local agenthost socket:
// <StateRoot>/agenthost/<conversationID>.sock. Created when the run's
// sandbox comes up and removed when the daemon closes.
func AgentHostSocketPath(conversationID string) string {
	return filepath.Join(AgentHostSocketDir(), sanitizePathSegment(conversationID)+".sock")
}

// sanitizePathSegment reduces an id to a filesystem-safe single segment.
// Conversation ids are UUIDs in every production caller, so this is defense in
// depth against a future caller with a less constrained shape — never a path
// separator, never a climb out of the parent.
func sanitizePathSegment(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, id)
	if clean == "" {
		return "unnamed"
	}
	return clean
}

// --- Class 3: local-only -------------------------------------------------

// DBPath is the SQLite database path: <StateRoot>/triagefactory.db.
// Local mode only — multi mode is Postgres-backed and never opens this.
func DBPath() string {
	return filepath.Join(StateRoot(), "triagefactory.db")
}

// --- home resolution (confined to this package) --------------------------

// ExpandHome expands a leading "~" or "~/" in p against the user's home
// directory and returns every other form unchanged. It exists so the
// few callers that legitimately resolve a non-state, home-relative path
// — the install/uninstall binary destination — can do so without
// reaching for os.UserHomeDir
// directly and tripping the forbidigo guard. Persistent TF state goes
// through the resolvers above; this is only for those one-off
// user-facing paths.
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
