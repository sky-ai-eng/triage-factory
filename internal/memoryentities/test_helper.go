package memoryentities

import "go.opentelemetry.io/otel/metric"

// TestT is the minimal slice of *testing.T / *testing.B that
// SetMeterProviderForTest needs. Defining it locally keeps the standard
// library's "testing" package out of production builds of this package.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetMeterProviderForTest points the skipped-link counter at provider for the
// duration of t, and restores the previous one via t.Cleanup. It lives in a
// non-_test.go file so the other writers of memory links (the exec funnel's
// touches) can read what their tests recorded. It swaps a process global, so
// tests that call it must not run in parallel with each other.
func SetMeterProviderForTest(t TestT, provider metric.MeterProvider) {
	t.Helper()
	prev := linkStats.Swap(newStats(provider))
	t.Cleanup(func() { linkStats.Store(prev) })
}
