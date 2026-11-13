//go:build !windows

package worktree

import "strings"

// encodeClaudeProjectDir returns the directory name Claude Code uses under
// ~/.claude/projects/ for a symlink-resolved absolute cwd.
//
// Encoding rule (verified empirically against Claude Code 2.1.119): every '/'
// AND every '.' in the resolved path becomes '-'. The dot replacement is the
// part that is easy to miss: a resolved cwd like
// `/Users/.../.triagefactory/...` encodes to `-Users-...--triagefactory-...`
// (note the `--` where the dot got collapsed), not
// `-Users-...-.triagefactory-...`.
//
// Only `/` and `.` are verified. The paths Triage Factory uses
// (/tmp/triagefactory-runs/<uuid>) contain nothing else; if a cwd is ever
// configured to a path with other special characters, revisit.
func encodeClaudeProjectDir(resolvedAbs string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(resolvedAbs)
}

// claudeEncodedNameTooLong is always false off Windows: no length limit is
// applied to the encoding there.
func claudeEncodedNameTooLong(string) bool { return false }
