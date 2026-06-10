package worktree

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// claudeProjectsDir is where Claude Code auto-creates per-cwd session history.
const claudeProjectsDir = ".claude/projects"

// claudeHome resolves the user's real home directory for ~/.claude
// access. Claude Code SDK session state (the per-cwd JSONL transcripts
// under claudeProjectsDir) is keyed to the agent's real HOME, not to TF
// state — in the jail HOME=/work handles it, on the host it's the
// user's ~/.claude. It therefore stays home-relative even in multi mode
// (where TF state diverges onto a mounted volume) and does NOT route
// through internal/paths. The single nolint here is the documented
// exception to the forbidigo guard for every ~/.claude site in this
// file.
func claudeHome() (string, error) {
	return os.UserHomeDir() //nolint:forbidigo // Claude Code SDK session state, not TF state (see internal/paths doc).
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

	home, err := claudeHome()
	if err != nil {
		return
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolved))
	if err := os.RemoveAll(projectDir); err != nil {
		log.Printf("[worktree] remove ghost project dir %s: %v", projectDir, err)
	}
}
