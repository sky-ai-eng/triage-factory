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

// TestPerRunBands_Disjoint pins the reserved-range invariant the package's
// own init() assertion enforces at load time (a regression there would panic
// on import, failing every test in the package) — asserted here too as an
// explicit, readable property: no sidecar uid is a jail uid, the parked
// run-tree owner is neither, and both bands stay clear of the orchestrator's
// default uid.
func TestPerRunBands_Disjoint(t *testing.T) {
	for idx := 0; idx < MaxSandboxes; idx++ {
		sc, jail := SidecarUID(uint8(idx)), JailUID(uint8(idx))
		if IsJailUID(sc) {
			t.Fatalf("sidecar uid %d (index %d) falls inside the jail band", sc, idx)
		}
		if IsSidecarUID(jail) {
			t.Fatalf("jail uid %d (index %d) falls inside the sidecar band", jail, idx)
		}
		if sc == 10001 || jail == 10001 {
			t.Fatalf("index %d derives the orchestrator's default uid", idx)
		}
	}
	if IsJailUID(ParkedRunTreeUID) || IsSidecarUID(ParkedRunTreeUID) {
		t.Fatalf("ParkedRunTreeUID %d falls inside a per-run band", ParkedRunTreeUID)
	}
}

// TestJailIdentity_DerivedPerIndex pins the derivation every grant names: one
// identity per subnet index, gid equal to uid, the sidecar at an index naming
// the jail gid of the same index, and nothing outside the sidecar band naming
// one at all.
func TestJailIdentity_DerivedPerIndex(t *testing.T) {
	seen := map[int]bool{}
	for idx := 0; idx < MaxSandboxes; idx++ {
		uid := JailUID(uint8(idx))
		if seen[uid] {
			t.Fatalf("jail uid %d derived twice", uid)
		}
		seen[uid] = true
		if JailGID(uint8(idx)) != uid {
			t.Errorf("JailGID(%d) = %d, want %d", idx, JailGID(uint8(idx)), uid)
		}
		if got, ok := SidecarJailGID(SidecarUID(uint8(idx))); !ok || got != JailGID(uint8(idx)) {
			t.Errorf("SidecarJailGID(SidecarUID(%d)) = %d, %v; want %d, true", idx, got, ok, JailGID(uint8(idx)))
		}
	}
	for _, uid := range []int{0, 10001, JailUID(3), SidecarUIDBase - 1, SidecarUIDBase + MaxSandboxes} {
		if _, ok := SidecarJailGID(uid); ok {
			t.Errorf("SidecarJailGID(%d) named a jail gid for a uid outside the sidecar band", uid)
		}
	}
}
