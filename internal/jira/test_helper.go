package jira

import "time"

// TestT is the slice of *testing.T the test seams here need, declared locally
// so production builds of this package never import the testing package.
// *testing.T satisfies it structurally.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetRetryBackoffForTest sets the first wait of the backoff a retried request
// takes when its response carries no Retry-After, for the duration of t, and
// restores it via t.Cleanup. It lets a test in another package serve 5xx
// responses through a real Client without sleeping through the production
// backoff. It mutates process-wide state, so tests that call it must not run
// in parallel with each other.
//
// Lives in a non-_test.go file so consumers' test packages can call it.
func SetRetryBackoffForTest(t TestT, base time.Duration) {
	t.Helper()
	prev := rateLimitBackoffBase
	rateLimitBackoffBase = base
	t.Cleanup(func() { rateLimitBackoffBase = prev })
}
