package sandbox

import "testing"

// TestSidecarUID_DerivesFromIndex pins the formula every caller (the Linux
// dispatcher, the broker's boundary validation) relies on: a contiguous,
// deterministic mapping from subnet index to sidecar uid/gid.
func TestSidecarUID_DerivesFromIndex(t *testing.T) {
	cases := []struct {
		idx  uint8
		want int
	}{
		{0, SidecarUIDBase},
		{1, SidecarUIDBase + 1},
		{255, SidecarUIDBase + 255},
	}
	for _, c := range cases {
		if got := SidecarUID(c.idx); got != c.want {
			t.Errorf("SidecarUID(%d) = %d, want %d", c.idx, got, c.want)
		}
	}
}

// TestSidecarUID_BandDisjointFromWorktreeUID pins the reserved-range
// invariant the package's own init() assertion enforces at load time (a
// regression there would panic on import, failing every test in the
// package) — asserted here too as an explicit, readable property.
func TestSidecarUID_BandDisjointFromWorktreeUID(t *testing.T) {
	if SidecarUIDBase < WorktreeUID+MaxSandboxes {
		t.Fatalf("SidecarUIDBase %d overlaps the WorktreeUID band [%d, %d)", SidecarUIDBase, WorktreeUID, WorktreeUID+MaxSandboxes)
	}
	// The full sidecar band [base, base+MaxSandboxes) must stay clear of
	// WorktreeUID too, not just its own base.
	for idx := 0; idx < MaxSandboxes; idx++ {
		if SidecarUIDBase+idx == WorktreeUID {
			t.Fatalf("sidecar uid %d collides with WorktreeUID", SidecarUIDBase+idx)
		}
	}
}
