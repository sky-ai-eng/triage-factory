package linear

import "time"

// TestT is the slice of *testing.T the test seams here need, declared locally
// so production builds of this package never import the testing package.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetRetryBackoffForTest sets the first wait of the backoff after a transient
// failure for the duration of t, and restores it via t.Cleanup, so a test in
// another package can drive a real Client through failures without sleeping
// through the production backoff. It mutates process-wide state, so tests
// that call it must not run in parallel with each other.
func SetRetryBackoffForTest(t TestT, base time.Duration) {
	t.Helper()
	prev := backoffBase
	backoffBase = base
	t.Cleanup(func() { backoffBase = prev })
}
