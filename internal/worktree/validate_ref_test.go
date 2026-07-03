package worktree

import "testing"

// TestValidateCheckoutRef pins the shared checkout-ref rule at its home (the
// interpolation point both `workspace add --ref` and the agenthost RPC route
// through). Beyond the conservative alphabet, it must catch the shapes the
// alphabet admits but git-check-ref-format still refuses — consecutive
// slashes, dot-leading components, ".lock" component suffixes, a trailing dot
// — so an accepted ref never turns into an opaque git fetch failure.
func TestValidateCheckoutRef(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
	}{
		{"main", true},
		{"SKY-220", true},
		{"feature/foo", true},
		{"release/1.2.x", true},
		{"a", true},
		{"user/nested/deep-branch", true},

		{"", false},
		{"-foo", false}, // leading dash → could be a git flag
		{"--upload-pack=evil", false},
		{"foo;bar", false},
		{"foo bar", false},
		{"foo..bar", false}, // illegal refname / path traversal
		{"foo\nbar", false},
		{"$(whoami)", false},
		{"foo:bar", false},
		{"foo~1", false},
		{"refs/heads/main", false}, // would double up into +refs/heads/refs/heads/main
		{"feature/", false},        // trailing slash
		{"foo//bar", false},        // empty component (consecutive slashes)
		{"a/.b", false},            // component starting with "."
		{".hidden", false},         // leading dot (also caught by the first-char class)
		{"a/b.lock", false},        // component ending in ".lock"
		{"a.lock/b", false},
		{"foo.", false}, // trailing dot
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			err := ValidateCheckoutRef(c.in)
			if c.wantOK && err != nil {
				t.Errorf("ValidateCheckoutRef(%q) = %v, want nil", c.in, err)
			}
			if !c.wantOK && err == nil {
				t.Errorf("ValidateCheckoutRef(%q) = nil, want error", c.in)
			}
		})
	}
}
