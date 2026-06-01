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
//   - Class 1, org-scoped persistent — the bare clone cache and the
//     curator project working dirs. These gain an /orgs/<orgID>/
//     segment in multi mode so each tenant's clones and knowledge bases
//     live under their own subtree. Resolved through OrgRoot.
//   - Class 2, host-global persistent — the sandbox rootfs cache and the
//     Agent SDK install. Shared read-only across every tenant by design;
//     org-scoping them would re-multiply identical toolchains per org
//     and defeat page-cache sharing, so they hang off StateRoot
//     directly, never OrgRoot.
//   - Class 3, local-only — the SQLite DB and takeover dirs. Multi mode
//     uses Postgres and defers takeover to v2, so these hang off
//     StateRoot with no org segment.
//
// os.UserHomeDir and the ".triagefactory" path literal live ONLY in
// this package; a forbidigo rule (.golangci.yml) plus a lint.sh grep
// guard keep every other package routing through these resolvers. The
// documented exceptions are ~/.claude state — Claude Code SDK session
// dirs, which must follow the real HOME even in multi mode where TF
// state diverges onto a mounted volume — and, until SKY-402 lands, the
// curator runtime (internal/curator/knowledge.go), which still owns its
// own home resolution.
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
// is the hook SKY-407's shared-nothing executors lean on).
const defaultMultiStateRoot = "/data"

// envStateRoot overrides the multi-mode state-root base.
const envStateRoot = "TF_STATE_ROOT"

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
// state lives.
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
	testRootMu.RLock()
	tr := testRoot
	testRootMu.RUnlock()
	if tr != "" {
		return tr
	}
	if runmode.Current() == runmode.ModeMulti {
		if env := strings.TrimSpace(os.Getenv(envStateRoot)); env != "" {
			return env
		}
		return defaultMultiStateRoot
	}
	return filepath.Join(mustHome(), dirName)
}

// OrgRoot returns the per-tenant subtree root for org-scoped state.
//
// In multi mode with a real org it is <StateRoot>/orgs/<orgID>. In
// local mode, OR when orgID is the local-default sentinel, the org
// segment is stripped and OrgRoot collapses to StateRoot. That strip is
// what keeps existing local installs byte-for-byte on disk and lets
// callers that aren't org-threaded yet pass runmode.LocalDefaultOrgID
// today without changing the local layout — SKY-406 threads the real
// orgID through the worktree cache afterward.
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
// <OrgRoot>/repos/<owner>/<repo>.git. SKY-406 consumes this as it
// threads a real orgID through the bounded, evictable worktree cache.
func BareCacheDir(orgID, owner, repo string) string {
	return filepath.Join(BareCacheRoot(orgID), owner, repo+".git")
}

// ProjectsRoot is the parent of every curator project working dir for
// an org: <OrgRoot>/projects.
func ProjectsRoot(orgID string) string {
	return filepath.Join(OrgRoot(orgID), "projects")
}

// ProjectKBDir is one curator project's working directory — the
// knowledge base plus its pinned worktrees: <OrgRoot>/projects/<id>.
// SKY-402 routes the curator runtime's KnowledgeDir through here.
func ProjectKBDir(orgID, projectID string) string {
	return filepath.Join(ProjectsRoot(orgID), projectID)
}

// --- Class 2: host-global persistent (NEVER org-scoped) ------------------

// SandboxRootfsDir is the cached sandbox rootfs for a toolchain cache
// key: <StateRoot>/sandbox/rootfs-<cacheKey>. Mounted read-only and
// shared across all tenants, hence StateRoot rather than OrgRoot —
// org-scoping it would re-extract an identical multi-hundred-MB
// toolchain per org and defeat page-cache sharing.
func SandboxRootfsDir(cacheKey string) string {
	return filepath.Join(StateRoot(), "sandbox", "rootfs-"+cacheKey)
}

// SDKDir is the Agent SDK install: <StateRoot>/sdk. The toolchain is
// identical for every tenant, so it is host-global, not org-scoped.
func SDKDir() string {
	return filepath.Join(StateRoot(), "sdk")
}

// --- Class 3: local-only -------------------------------------------------

// DBPath is the SQLite database path: <StateRoot>/triagefactory.db.
// Local mode only — multi mode is Postgres-backed and never opens this.
func DBPath() string {
	return filepath.Join(StateRoot(), "triagefactory.db")
}

// TakeoversRoot is the default base for takeover worktrees:
// <StateRoot>/takeovers. Local-mode UX; multi defers takeover to v2.
// A user-configured override (instance_config.server_takeover_dir) is
// resolved by the caller via ExpandHome, not here.
func TakeoversRoot() string {
	return filepath.Join(StateRoot(), "takeovers")
}

// --- home resolution (confined to this package) --------------------------

// ExpandHome expands a leading "~" or "~/" in p against the user's home
// directory and returns every other form unchanged. It exists so the
// few callers that legitimately resolve a non-state, home-relative path
// — the install/uninstall binary destination, a user-configured
// takeover override — can do so without reaching for os.UserHomeDir
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

// mustHome resolves the user's home directory or panics. os.UserHomeDir
// fails only when $HOME (or its Windows equivalent) is unset — a state
// in which a local-mode TF binary cannot locate its DB, config, or
// keychain entries and therefore cannot run at all. Rather than thread
// an error return through every resolver (the API is deliberately
// error-free: these are pure path joins), we surface that unreachable
// condition as a loud panic at the single resolution site. Multi mode
// and TF_STATE_ROOT never reach this; tests use SetForTest; every real
// local launch — desktop app, CLI, CI — has $HOME set.
func mustHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Sprintf("paths: cannot resolve home directory (is $HOME set?): %v", err))
	}
	return home
}
