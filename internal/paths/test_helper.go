package paths

// TestT is the minimal slice of *testing.T / *testing.B that SetForTest
// needs. Defining it locally lets this file decline to import the
// standard-library "testing" package, keeping production builds of
// internal/paths clean of that surface area. *testing.T and *testing.B
// both satisfy it structurally, so callers pass them directly.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetForTest pins the state root to root for the duration of t and
// restores the previous override via t.Cleanup. This is the state-root
// analogue of runmode.SetForTest and is deliberately separate: a test
// can pin where state lands without touching the runtime mode, and a
// mode-flipping test need not also relocate the root. Access goes
// through testRootMu so it's data-race-free — but, like
// runmode.SetForTest, it mutates a process global and so is not safe
// for overlapping parallel tests; callers must serialize their use.
//
// Lives in a non-_test.go file so other packages' test code (db,
// worktree, sandbox, ...) can relocate TF state onto a t.TempDir()
// instead of the developer's real ~/.triagefactory. The local TestT
// interface keeps the testing-package import out of production builds.
func SetForTest(t TestT, root string) {
	t.Helper()
	testRootMu.Lock()
	prev := testRoot
	testRoot = root
	testRootMu.Unlock()
	t.Cleanup(func() {
		testRootMu.Lock()
		testRoot = prev
		testRootMu.Unlock()
	})
}
