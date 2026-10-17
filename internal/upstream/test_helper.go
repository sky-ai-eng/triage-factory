package upstream

import "go.opentelemetry.io/otel/metric"

// TestT is the minimal slice of *testing.T / *testing.B that
// SetMeterProviderForTest needs. Defining it locally keeps the standard
// library's "testing" package out of production builds of this package.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetMeterProviderForTest points the counters, the query-cost histogram and
// the rate-limit gauges at provider for the duration of t, and restores the
// previous ones via t.Cleanup. It lives in a non-_test.go file so a client's
// own tests can read what its requests recorded. It swaps a process global, so tests that call it must
// not run in parallel with each other.
func SetMeterProviderForTest(t TestT, provider metric.MeterProvider) {
	t.Helper()
	prev := current()
	global.Store(newInstruments(provider))
	t.Cleanup(func() { global.Store(prev) })
}

// ResetBudgetsForTest gives t an empty rate-limit budget registry, and
// restores the previous one via t.Cleanup. Like SetMeterProviderForTest it
// swaps a process global, so tests that call it must not run in parallel with
// each other.
func ResetBudgetsForTest(t TestT) {
	t.Helper()
	prev := liveBudgets.Swap(newBudgetRegistry())
	t.Cleanup(func() { liveBudgets.Store(prev) })
}

// BudgetForTest returns the latest reading RecordBudget kept for the key, so a
// client's own tests can read what its responses reported.
func BudgetForTest(name Name, orgID, credential, bucket string) (Budget, bool) {
	return liveBudgets.Load().get(budgetKey{name, orgID, credential, bucket})
}
