package tooldefs

import "sync"

// TestT is the slice of *testing.T that OfferForTest needs, declared here so
// this file does not import the testing package into production builds.
type TestT interface {
	Helper()
	Cleanup(func())
}

var (
	testToolsMu sync.Mutex
	testTools   []Tool
)

// OfferForTest adds tool to the TF tools every conversation is offered, after
// the shipped ones, until t ends. It exists so another package's tests can
// drive a runtime's TF-tool dispatch with a handler that records what it was
// called with. It changes process state, so tests that call it must not run
// in parallel with tests that read the offered set.
//
// Lives in a non-_test.go file so other packages' tests can call it.
func OfferForTest(t TestT, tool Tool) {
	t.Helper()
	testToolsMu.Lock()
	prev := testTools
	testTools = append(append([]Tool(nil), prev...), tool)
	testToolsMu.Unlock()
	t.Cleanup(func() {
		testToolsMu.Lock()
		testTools = prev
		testToolsMu.Unlock()
	})
}

func offeredForTest() []Tool {
	testToolsMu.Lock()
	defer testToolsMu.Unlock()
	return testTools
}
