//go:build linux

package sandbox

import "testing"

// TestRunNetFromNetnsName: a namespace name parses only if it is exactly one
// runNetNames builds, and then yields that network's names.
func TestRunNetFromNetnsName(t *testing.T) {
	for _, idx := range []uint8{0, 7, 255} {
		name := NetnsNameForRun("conv-a", idx)
		names, got, ok := runNetFromNetnsName(name)
		if !ok || got != idx {
			t.Fatalf("runNetFromNetnsName(%q) = _, %d, %v; want index %d", name, got, ok, idx)
		}
		if want := runNetNames(netnsIDFrag("conv-a"), idx); *names != *want {
			t.Errorf("runNetFromNetnsName(%q) = %+v, want %+v", name, *names, *want)
		}
	}
	frag := netnsIDFrag("conv-a")
	for _, name := range []string{
		"",
		"default",
		"tf-" + frag + "-256",
		"tf-" + frag + "-07",
		"tf-" + frag[:7] + "-7",
		"tf-" + frag + "0-7",
		"tf-DEADBEEF-7",
		"../tf-" + frag + "-7",
		"tf-" + frag + "-7/..",
	} {
		if _, _, ok := runNetFromNetnsName(name); ok {
			t.Errorf("runNetFromNetnsName accepted %q", name)
		}
	}
}
