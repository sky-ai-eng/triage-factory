package dbtest

import "time"

// awaitClockTick blocks until the wall clock reads later than it did on entry.
// The stores stamp rows with time.Now, and some cases assert a strict order
// between writes made microseconds apart; a platform whose wall clock ticks
// every millisecond or two (Windows) would otherwise hand two writes one
// timestamp.
func awaitClockTick() {
	start := time.Now().Round(0)
	for !time.Now().Round(0).After(start) {
		time.Sleep(time.Millisecond)
	}
}
