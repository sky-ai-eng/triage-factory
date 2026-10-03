package suspendclock

import (
	"time"

	"golang.org/x/sys/unix"
)

// platformSuspended is CLOCK_BOOTTIME minus CLOCK_MONOTONIC. The kernel
// defines BOOTTIME as MONOTONIC plus the time spent suspended, and NTP slews
// the two together, so the difference is suspended time and nothing else: a
// wall-clock step moves neither.
//
// MONOTONIC is read first so the gap between the two calls can only bias the
// reading up by the call's own duration, never read a suspend as negative.
func platformSuspended() (time.Duration, bool) {
	var mono, boot unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return 0, false
	}
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot); err != nil {
		return 0, false
	}
	return time.Duration(boot.Nano() - mono.Nano()), true
}
