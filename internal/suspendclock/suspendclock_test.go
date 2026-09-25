package suspendclock

import (
	"runtime"
	"testing"
	"time"
)

// TestSuspended_ReportsOnLinux reads the real clocks. CI cannot suspend the
// machine, so this pins only that the reading is available and sane: a
// cumulative suspended time is never negative.
func TestSuspended_ReportsOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the reading this test pins is Linux's")
	}
	d, ok := Suspended()
	if !ok {
		t.Fatal("Suspended() is not ok on Linux; CLOCK_BOOTTIME and CLOCK_MONOTONIC are both always available")
	}
	if d < 0 {
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
