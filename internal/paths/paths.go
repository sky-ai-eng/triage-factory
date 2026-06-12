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
//   - Class 3, local-only — the SQLite DB. Multi mode uses Postgres, so
//     this hangs off StateRoot with no org segment.
//
// os.UserHomeDir and the ".triagefactory" path literal live ONLY in
// this package; a forbidigo rule (.golangci.yml) plus a lint.sh grep
// guard keep every other package routing through these resolvers. The
// one documented exception is ~/.claude state — Claude Code SDK session
// dirs, which must follow the real HOME even in multi mode where TF
// state diverges onto a mounted volume. (The curator runtime resolved
// its own home until SKY-402 routed KnowledgeDir/ProjectsRoot through
// ProjectKBDir/ProjectsRoot here.)
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
// sandbox/SDK caches, the worktree bare cache, projectbundle, …) route
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

// CuratorSharedReposRoot is the parent of the per-(org, repo) SHARED curator
// worktrees: <OrgRoot>/curator-repos. In multi mode the Curator materializes
// one worktree per pinned repo here and bind-mounts it read-only into every
// member's sandbox, so N concurrent sessions on the same repo share ~1x
// storage instead of a per-session checkout (TFAC-61). Org-scoped (never
// host-global) by design: unlike the shared read-only rootfs cache (Class 2),
// a repo's checked-out files are tenant data and must not cross the org
// boundary even read-only — the shared mount is safe precisely because it is
// one trust domain (a single org) plus the ro mount option.
func CuratorSharedReposRoot(orgID string) string {
	return filepath.Join(OrgRoot(orgID), "curator-repos")
}

// CuratorSharedRepoDir is the shared worktree path for one pinned repo:
// <OrgRoot>/curator-repos/<owner>/<repo>. The nested <owner>/<repo> leaf
// mirrors the per-project layout under ProjectKBDir so the agent's
// ./repos/<owner>/<repo> mental model is identical whether the tree is a
// per-project worktree (local) or a shared mount (multi).
func CuratorSharedRepoDir(orgID, owner, repo string) string {
	return filepath.Join(CuratorSharedReposRoot(orgID), owner, repo)
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
