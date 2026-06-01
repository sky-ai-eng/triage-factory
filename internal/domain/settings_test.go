package domain

import "testing"

// TestEffectiveCloneProtocol pins the mode-aware resolution (SKY-391): multi
// mode is always https regardless of the stored value (SSH is unavailable in
// a hosted runtime), while local mode honors only the literal "ssh" and
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
		// DefaultOrgSettings ships "ssh" — correct for local, must coerce in multi.
		{"local default-row ssh stays ssh", DefaultOrgSettings().GitHubCloneProtocol, false, "ssh"},
		{"multi default-row ssh coerces", DefaultOrgSettings().GitHubCloneProtocol, true, "https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveCloneProtocol(tc.stored, tc.multiMode); got != tc.want {
				t.Errorf("EffectiveCloneProtocol(%q, multi=%v) = %q, want %q", tc.stored, tc.multiMode, got, tc.want)
			}
		})
	}
}
