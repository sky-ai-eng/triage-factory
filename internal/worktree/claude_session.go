package worktree

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
)

// claudeProjectsDir is where Claude Code auto-creates per-cwd session history.
const claudeProjectsDir = ".claude/projects"

// claudeHome resolves this process's home directory for ~/.claude
// access. Claude Code SDK session state (the per-cwd JSONL transcripts
// under claudeProjectsDir) is keyed to the HOME of the process that ran
// the agent — which is this process's HOME only for DIRECT runs (local
// mode, or multi on non-Linux). Sandboxed runs execute with HOME=/work
// and never touch this process's home; ClaudeProjectDir owns that
// branch. This helper must only be called on the direct-run branch, and
// the nolint below is the documented exception to the forbidigo guard
// for every ~/.claude site in this file.
func claudeHome() (string, error) {
	return os.UserHomeDir() //nolint:forbidigo // Claude Code SDK session state, not TF state (see internal/paths doc).
}

// ClaudeProjectDir returns the host-side directory Claude Code wrote (or
// will write) its per-cwd session state into — the dir holding
// `<sessionID>.jsonl` plus the `<sessionID>/subagents` and
// `<sessionID>/tool-results` trees — for a run whose symlink-resolved
// host cwd is resolvedCwd.
//
// Direct runs (local mode, or multi on non-Linux) inherit this process's
// HOME, so the tree is ~/.claude/projects/<encode(resolvedCwd)>.
//
// Sandboxed runs (multi + Linux) execute with HOME=/work and cwd=/work,
// where /work bind-mounts resolvedCwd. The agent therefore writes
// /work/.claude/projects/<encode("/work")>/... — which on the host is
// <resolvedCwd>/.claude/projects/-work/... . Tenant scoping there is
// structural: the transcript lives inside the run's own (org-scoped)
// directory, and the orchestrator's $HOME is never involved. Host-side
// consumers (delegate snapshot/resume) MUST resolve through this function
// rather than assuming the home-relative layout, or they silently miss
// every sandboxed transcript.
func ClaudeProjectDir(resolvedCwd string) (string, error) {
	if agentproc.WillSandbox() {
		return filepath.Join(resolvedCwd, claudeProjectsDir, encodeClaudeProjectDir(agentproc.SandboxWorkRoot)), nil
	}
	home, err := claudeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolvedCwd)), nil
}

// encodeClaudeProjectDir returns the directory name Claude Code uses
// under ~/.claude/projects/ for a symlink-resolved absolute cwd.
//
// Encoding rule (verified empirically against Claude Code 2.1.119):
// every '/' AND every '.' in the resolved path becomes '-'. The
// dot-replacement is the part that's easy to miss — paths like
// ~/.triagefactory/... contain dots, and only replacing slashes
// produces a name Claude Code can't find: a resolved cwd like
// `/Users/.../.triagefactory/...` encodes to
// `-Users-...--triagefactory-...` (note the `--` where the dot got
// collapsed), not `-Users-...-.triagefactory-...`.
//
// Caveat: only `/` and `.` are verified. Claude Code may also rewrite
// other characters (underscores, spaces). The paths Triage Factory
// actually uses (/tmp/triagefactory-runs/<uuid>) only contain slashes
// and dots, so this matches in practice; if a cwd is ever configured
// to a path with other special characters, revisit.
func encodeClaudeProjectDir(resolvedAbs string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(resolvedAbs)
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

// ResolveClaudeProjectCwd returns the symlink-resolved absolute path
// the way Claude Code records cwds for project-dir naming. Callers
// capture this for a worktree BEFORE it is moved/removed so the
// resolved value can be used to locate the session JSONL the agent
// wrote under ~/.claude/projects.
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

	// Sandboxed runs write their session tree INSIDE the run root
	// (<cwd>/.claude/projects/-work — see ClaudeProjectDir), so it is
	// deleted with the worktree itself and no host-side ~/.claude entry
	// ever exists to clean up.
	if agentproc.WillSandbox() {
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
		worktreeLog.Warn("refusing to clean project dir for non-tmp cwd", "cwd", resolved)
		return
	}

	home, err := claudeHome()
	if err != nil {
		return
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.RemoveAll(projectDir); err != nil {
		worktreeLog.Warn("remove ghost project dir", "dir", projectDir, "error", err)
	}
}
