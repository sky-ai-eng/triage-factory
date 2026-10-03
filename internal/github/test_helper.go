package github

import "time"

// TestT is the slice of *testing.T the test seams here need, declared locally
// so production builds of this package never import the testing package.
// *testing.T satisfies it structurally.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetTransientBackoffForTest sets the first wait before a transient failure is
// retried for the duration of t, and restores it via t.Cleanup. It lets a test
// in another package serve 5xx responses through a real Client without
// sleeping through the production backoff. It mutates process-wide state, so
// tests that call it must not run in parallel with each other.
//
// Lives in a non-_test.go file so consumers' test packages can call it.
func SetTransientBackoffForTest(t TestT, base time.Duration) {
	t.Helper()
	prev := transientBackoffBase
	transientBackoffBase = base
	t.Cleanup(func() { transientBackoffBase = prev })
}
