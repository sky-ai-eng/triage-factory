// Package suspendclock reports how long the system has spent suspended
// since boot, from two kernel clocks that differ only by suspended time.
// It is a cumulative reading: callers keep a base and subtract.
//
// Go's monotonic clock stops while the system sleeps on every platform TF
// ships, so a timer or a time.Since cannot see a suspend at all. That is
// the right answer for measuring what a process did, and the wrong one for
// comparing against a lease stamped on database time, which kept running.
// This package is the one reading that tells the two apart.
//
// Readings are not smoothed. The two clocks are read in two calls, so two
// readings taken close together can step backwards by a sub-microsecond
// skew; callers clamp the difference at zero.
package suspendclock

import (
	"sync/atomic"
	"time"
)

// source is the reading Suspended returns: the platform's by default,
// swapped by SetSourceForTest. Atomic because an engagement's renewal loop
// reads it on its own goroutine while a test may be swapping it.
var source atomic.Pointer[func() (time.Duration, bool)]

func init() {
	read := platformSuspended
	source.Store(&read)
}

// Suspended returns the system's cumulative suspended time and whether
// this platform can report it. ok is false where it cannot; callers treat
// that as "no suspend observed", which is the behavior before this package.
func Suspended() (d time.Duration, ok bool) {
	return (*source.Load())()
}

// TestT is the slice of *testing.T that SetSourceForTest needs, declared
// here so production builds of this package do not import "testing".
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetSourceForTest replaces the reading Suspended returns for the duration
// of t and restores the previous one on cleanup. It lives outside a
// _test.go file so other packages' tests can fake a suspend. It mutates a
// process global, so tests that use it must not run in parallel with each
// other.
func SetSourceForTest(t TestT, fn func() (time.Duration, bool)) {
	t.Helper()
	prev := source.Swap(&fn)
	t.Cleanup(func() { source.Store(prev) })
}
