//go:build windows

package worktree

import (
	"strings"
	"unicode/utf16"
)

// claudeEncodedNameMax is the longest encoded project-dir name Claude Code
// accepts as-is. Beyond it Claude Code truncates the name and appends a hash
// computed by JavaScript string arithmetic that TF does not reproduce, so TF
// cannot name the directory such a cwd gets.
const claudeEncodedNameMax = 200

// encodeClaudeProjectDir returns the directory name Claude Code uses under
// ~/.claude/projects/ for a symlink-resolved absolute cwd.
//
// On Windows every character outside [A-Za-z0-9] becomes '-' and case is
// preserved: `C:\Users\me\.x` encodes to `C--Users-me--x`. Claude Code
// iterates JavaScript string units, which are UTF-16 code units, so a rune
// outside the basic plane (a surrogate pair) contributes two dashes, not one.
func encodeClaudeProjectDir(resolvedAbs string) string {
	units := utf16.Encode([]rune(resolvedAbs))
	var b strings.Builder
	b.Grow(len(units))
	for _, u := range units {
		switch {
		case u >= 'a' && u <= 'z', u >= 'A' && u <= 'Z', u >= '0' && u <= '9':
			b.WriteByte(byte(u))
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// claudeEncodedNameTooLong reports whether an encoded name is one Claude Code
// would have truncated and hashed.
func claudeEncodedNameTooLong(encoded string) bool {
	return len(encoded) > claudeEncodedNameMax
}
