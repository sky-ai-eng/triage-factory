package worktree

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
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
		return filepath.Join(resolvedCwd, claudeProjectsDir, encodeClaudeProjectDir(cell.WorkDir)), nil
	}
	home, err := claudeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolvedCwd)), nil
}

// claudeProjectNameMax is the longest project directory name Claude Code
// writes as-is; a longer one is cut to this length and given a hash suffix.
const claudeProjectNameMax = 200

// encodeClaudeProjectDir returns the directory name Claude Code uses
// under ~/.claude/projects/ for a symlink-resolved absolute cwd.
//
// Claude Code computes it in JavaScript, on every platform alike:
//
//	n = cwd.replace(/[^a-zA-Z0-9]/g, "-")
//	n.length <= 200 ? n : n.slice(0, 200) + "-" + Math.abs(h(cwd)).toString(36)
//
// where h is the 31-multiplier int32 string hash over the original cwd. A
// JavaScript string is UTF-16, so both the replacement and the hash run per
// UTF-16 unit, not per byte or per rune: a character outside the Basic
// Multilingual Plane is two units and becomes two dashes. Claude Code does not
// normalize the path first, so a decomposed "é" (e + U+0301) becomes "e-".
func encodeClaudeProjectDir(resolvedAbs string) string {
	units := utf16.Encode([]rune(resolvedAbs))
	name := make([]byte, len(units))
	var hash int32 // overflow wraps, as JavaScript's `| 0` does
	for i, u := range units {
		if isASCIIAlnum(u) {
			name[i] = byte(u)
		} else {
			name[i] = '-'
		}
		hash = hash*31 + int32(u)
	}
	if len(name) <= claudeProjectNameMax {
		return string(name)
	}
	// int64, so the absolute value of math.MinInt32 does not overflow.
	abs := int64(hash)
	if abs < 0 {
		abs = -abs
	}
	return string(name[:claudeProjectNameMax]) + "-" + strconv.FormatInt(abs, 36)
}

func isASCIIAlnum(u uint16) bool {
	return 'a' <= u && u <= 'z' || 'A' <= u && u <= 'Z' || '0' <= u && u <= '9'
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
// Claude Code auto-creates whenever it's invoked in a new cwd, so the entry for
// an ephemeral run root does not outlive the root. The entry holds the session
// transcripts `claude --resume` reads, so a caller removes it only for a cwd
// whose sessions are held elsewhere or no longer wanted.
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
