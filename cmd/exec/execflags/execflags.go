// Package execflags holds the argument scanning shared by every `triagefactory
// exec` verb surface. It is deliberately dependency-free — the verb packages
// (gh, jira, memory) import it, never the other way around.
package execflags

// HasHelpFlag reports whether --help or -h appears among args as a standalone
// flag rather than as the VALUE of a preceding flag. Every verb surface routes
// its help through this, so `exec gh pr --help`, `exec gh pr edit --help`, and
// `exec jira ticket view --help` all behave the same way — the native
// envelope tells agents to run --help on any subcommand, so the answer has to
// be the same at every depth.
//
// valueFlags is the caller's set of flags that TAKE a value. It is what stops
// `gh pr add-comment 12 --body "--help"` from printing usage instead of
// posting the comment: the literal "--help" there is the body's value, not a
// request. It must contain only value-taking flags — listing a boolean
// (`--draft`, `--stdout`) would make `--stdout --help` a false negative,
// swallowing a real help request. A nil or empty map means no flag takes a
// value, so every --help occurrence counts.
func HasHelpFlag(args []string, valueFlags map[string]bool) bool {
	for i, a := range args {
		if a != "--help" && a != "-h" {
			continue
		}
		// A preceding value-taking flag claims this token as its value. --help
		// itself never claims one, so an earlier --help can't shadow a later.
		if i > 0 && valueFlags[args[i-1]] {
			continue
		}
		return true
	}
	return false
}
