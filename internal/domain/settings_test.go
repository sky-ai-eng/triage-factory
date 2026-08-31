package domain

import "testing"

// TestEffectiveCloneProtocol pins the mode-aware resolution: multi
// mode is always https regardless of the stored value (SSH is unavailable in
// a multi-mode runtime), while local mode honors only the literal "ssh" and
// defaults everything else to https.
func TestEffectiveCloneProtocol(t *testing.T) {
	cases := []struct {
		name      string
		stored    string
		multiMode bool
		want      string
	}{
		{"multi ignores stored ssh", "ssh", true, "https"},
		{"multi ignores stored https", "https", true, "https"},
		{"multi ignores empty", "", true, "https"},
		{"multi ignores stale value", "garbage", true, "https"},
		{"local honors ssh", "ssh", false, "ssh"},
		{"local honors https", "https", false, "https"},
		{"local defaults empty to https", "", false, "https"},
		{"local defaults stale to https", "garbage", false, "https"},
		// The package default is what a read with no row resolves to and what
		// an explicit clear lands on, so it has to be a value BOTH modes can
		// honor — otherwise every door onto the column needs to fix it up
		// rather than none of them.
		{"package default survives local", DefaultOrgSettings().GitHubCloneProtocol, false, "https"},
		{"package default survives multi", DefaultOrgSettings().GitHubCloneProtocol, true, "https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveCloneProtocol(tc.stored, tc.multiMode); got != tc.want {
				t.Errorf("EffectiveCloneProtocol(%q, multi=%v) = %q, want %q", tc.stored, tc.multiMode, got, tc.want)
			}
		})
	}
}
