package suspendclock

import (
	"runtime"
	"testing"
	"time"
)

// TestSuspended_Reports reads the real clocks on each platform that has a
// pair to compare. CI cannot suspend the machine, so this pins only that the
// reading is available, and on Linux that it is sane: BOOTTIME is MONOTONIC
// plus suspended time, read second, so the reading is never negative. darwin's
// pair drifts by NTP's frequency correction and can sit slightly below zero on
// a machine that has not slept, so its sign pins nothing.
func TestSuspended_Reports(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("only Linux and darwin have a clock pair to read")
	}
	d, ok := Suspended()
	if !ok {
		t.Fatalf("Suspended() is not ok on %s; both clocks of its pair are always available", runtime.GOOS)
	}
	if runtime.GOOS == "linux" && d < 0 {
		t.Errorf("Suspended() = %s, want a non-negative cumulative reading", d)
	}
}

// TestSetSourceForTest_RoundTrips pins the seam: the fake is what Suspended
// returns while the test runs, and the platform reading comes back after.
func TestSetSourceForTest_RoundTrips(t *testing.T) {
	before := source.Load()
	t.Run("swapped", func(t *testing.T) {
		SetSourceForTest(t, func() (time.Duration, bool) { return 42 * time.Second, true })
		if d, ok := Suspended(); d != 42*time.Second || !ok {
			t.Errorf("Suspended() under the fake = (%s, %v), want (42s, true)", d, ok)
		}
	})
	if source.Load() != before {
		t.Error("SetSourceForTest did not restore the previous source on cleanup")
	}
}
