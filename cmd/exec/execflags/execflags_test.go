package execflags

import "testing"

// TestHasHelpFlag covers the three things the helper has to get right at once:
// help is recognized at any position (not just args[0]), a value-taking flag
// swallows the --help that follows it, and a BOOLEAN flag does not — the last
// being the correction the jira-local version needed before it could serve
// `gh pr`, which has several booleans.
func TestHasHelpFlag(t *testing.T) {
	valueFlags := map[string]bool{"--body": true, "--repo": true, "--severity": true}

	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"bare --help", []string{"--help"}, true},
		{"bare -h", []string{"-h"}, true},
		{"below the verb", []string{"edit", "--help"}, true},
		{"after a positional", []string{"7", "--help"}, true},
		{"after a boolean flag", []string{"--stdout", "--help"}, true},
		{"after another boolean", []string{"--draft", "-h"}, true},
		{"after a value flag is that flag's value", []string{"--body", "--help"}, false},
		{"a repo literally named --help", []string{"--repo", "--help"}, false},
		{"value flag then a real help request", []string{"--body", "text", "--help"}, true},
		{"--help does not claim the next --help", []string{"--help", "--help"}, true},
		{"no help at all", []string{"view", "7", "--repo", "o/r"}, false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasHelpFlag(tc.args, valueFlags); got != tc.want {
				t.Errorf("HasHelpFlag(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestHasHelpFlag_NilValueFlags pins the degenerate set: with no flag declared
// value-taking, every --help counts. This is the top-level dispatcher's
// behavior for a surface that declares no flags, and it must not panic.
func TestHasHelpFlag_NilValueFlags(t *testing.T) {
	if !HasHelpFlag([]string{"--body", "--help"}, nil) {
		t.Error("with no value flags declared, a --help anywhere is a help request")
	}
	if HasHelpFlag([]string{"list"}, nil) {
		t.Error("no --help present, want false")
	}
}
