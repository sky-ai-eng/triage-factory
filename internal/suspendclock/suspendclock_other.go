//go:build !linux && !darwin

package suspendclock

import "time"

// platformSuspended reports nothing on a platform with no pair of clocks to
// compare, which every caller reads as "no suspend observed".
func platformSuspended() (time.Duration, bool) {
	return 0, false
}
