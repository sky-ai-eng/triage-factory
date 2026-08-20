package dbtest

import "testing"

// AssertCountOnlyList is the acceptance mechanism for the CountOnly half of
// the list contract (db.ListOpts.CountOnly, httpx's explicit page_size: 0):
// run the same read twice — once windowed, once with CountOnly — and hand
// this the results. It asserts the count-only call returned no rows and the
// same filtered total the windowed read counted. An impl that ignores the
// flag fails the no-rows half by answering with an unwindowed fetch of every
// matching row, which is exactly the silent regression the flag's contract
// warns about.
func AssertCountOnlyList(t *testing.T, what string, countOnlyLen, countOnlyTotal, windowedTotal int) {
	t.Helper()
	if countOnlyLen != 0 {
		t.Errorf("%s: count-only read returned %d rows, want 0", what, countOnlyLen)
	}
	if countOnlyTotal != windowedTotal {
		t.Errorf("%s: count-only total = %d, want the windowed read's %d", what, countOnlyTotal, windowedTotal)
	}
}
