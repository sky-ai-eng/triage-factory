package upstream

import "go.opentelemetry.io/otel/metric"

// TestT is the minimal slice of *testing.T / *testing.B that
// SetMeterProviderForTest needs. Defining it locally keeps the standard
// library's "testing" package out of production builds of this package.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetMeterProviderForTest points the request and retry counters at provider
// for the duration of t, and restores the previous counters via t.Cleanup.
// It lives in a non-_test.go file so a client's own tests can read what its
// retry loop recorded. It swaps a process global, so tests that call it must
// not run in parallel with each other.
func SetMeterProviderForTest(t TestT, provider metric.MeterProvider) {
	t.Helper()
	prev := current()
	global.Store(newInstruments(provider))
	t.Cleanup(func() { global.Store(prev) })
}
