package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
)

// claudeProjectsDir is where Claude Code auto-creates per-cwd session history.
const claudeProjectsDir = ".claude/projects"

// ErrClaudeSessionNameTooLong is returned on Windows when the encoded project
// directory name for a cwd exceeds the length Claude Code stores verbatim. Past
// it Claude Code truncates the name and appends a hash TF does not reproduce, so
// the session transcript cannot be located. Callers treat it like a missing
// session: a crash-reclaim starts fresh, and a message resume reports the
// session lost, as Claude Code itself does for such a cwd.
var ErrClaudeSessionNameTooLong = errors.New("claude project directory name exceeds the length Claude Code stores verbatim")

// WarnIfClaudeSessionNameTooLong logs a WARN when cwd's encoded Claude project
// directory name is too long for TF to locate on this platform (Windows only).
// Call it at launch so the cause of a later failed resume is visible in the log
// of the run that created the problem.
func WarnIfClaudeSessionNameTooLong(cwd string) {
	encoded, resolved := claudeProjectEncoding(cwd)
	if !claudeEncodedNameTooLong(encoded) {
		return
	}
	worktreeLog.Warn("encoded cwd exceeds Claude Code's project directory name limit; session resume and cleanup are unavailable for this run, shorten the state root or TEMP path",
		"cwd", resolved, "encoded_len", len(encoded))
}

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
		return filepath.Join(resolvedCwd, claudeProjectsDir, encodeClaudeProjectDir(cell.WorkDir)), nil
	}
	home, err := claudeHome()
	if err != nil {
		return "", err
	}
	encoded := encodeClaudeProjectDir(resolvedCwd)
	if claudeEncodedNameTooLong(encoded) {
		worktreeLog.Warn("claude project dir name too long to locate", "cwd", resolvedCwd, "encoded_len", len(encoded))
		return "", fmt.Errorf("%w: %d characters for %q", ErrClaudeSessionNameTooLong, len(encoded), resolvedCwd)
	}
	return filepath.Join(home, claudeProjectsDir, encoded), nil
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
	encoded := encodeClaudeProjectDir(resolved)
	if claudeEncodedNameTooLong(encoded) {
		worktreeLog.Warn("not removing claude project dir: name too long to locate", "cwd", resolved, "encoded_len", len(encoded))
		return
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encoded)
	if err := os.RemoveAll(projectDir); err != nil {
		worktreeLog.Warn("remove ghost project dir", "dir", projectDir, "error", err)
	}
}
