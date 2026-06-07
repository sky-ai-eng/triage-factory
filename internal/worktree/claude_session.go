package worktree

import (
	"fmt"
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
	home, err := claudeHome()
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

	home, err := claudeHome()
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

	home, err := claudeHome()
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
	home, err := claudeHome()
	if err != nil {
		return
	}
	projectDir := filepath.Join(home, claudeProjectsDir, encodeClaudeProjectDir(resolvedCwd))
	if err := os.RemoveAll(projectDir); err != nil {
		log.Printf("[worktree] remove project dir %s: %v", projectDir, err)
	}
}
