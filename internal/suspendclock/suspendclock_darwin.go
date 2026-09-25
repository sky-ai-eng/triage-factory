package suspendclock

import (
	"time"

	"golang.org/x/sys/unix"
)

// platformSuspended is CLOCK_MONOTONIC minus CLOCK_UPTIME_RAW. Per Apple's
// clock_gettime(3), CLOCK_MONOTONIC keeps counting while the system sleeps
// and CLOCK_UPTIME_RAW (mach_absolute_time) does not, so the difference
// grows by exactly the time asleep.
//
// CLOCK_UPTIME_RAW is read first, for the reason the Linux reading orders
// its calls: the gap between them can only bias the reading up.
func platformSuspended() (time.Duration, bool) {
	var uptime, mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_UPTIME_RAW, &uptime); err != nil {
		return 0, false
	}
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return 0, false
	}
	return time.Duration(mono.Nano() - uptime.Nano()), true
}
