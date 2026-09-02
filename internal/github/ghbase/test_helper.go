package ghbase

// TestT is the slice of *testing.T that SetDefaultBaseURLForTest needs,
// declared locally so production builds of this leaf never import the testing
// package. *testing.T satisfies it structurally.
type TestT interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

// SetDefaultBaseURLForTest swaps the deployment's default GitHub for the
// duration of t and restores the previous (value, initialized) pair via
// t.Cleanup. base is held to the same rule the env var is. Data-race-free, but
// it mutates process-wide state, so tests that call it must not run in parallel
// with each other.
//
// Lives in a non-_test.go file so consumers' test packages can call it — a
// bind-ceremony test that points its fake GitHub here, a store test that pins
// what an unset host resolves to.
func SetDefaultBaseURLForTest(t TestT, base string) {
	t.Helper()
	canonical, err := ParseDefaultBaseURL(base)
	if err != nil {
		t.Fatalf("ghbase.SetDefaultBaseURLForTest: %v", err)
	}
	defaultBaseURLInitMu.Lock()
	prev, prevInit := defaultBaseURL, defaultBaseURLInit
	defaultBaseURL = canonical
	defaultBaseURLInit = true
	defaultBaseURLInitMu.Unlock()
	t.Cleanup(func() {
		defaultBaseURLInitMu.Lock()
		defaultBaseURL, defaultBaseURLInit = prev, prevInit
		defaultBaseURLInitMu.Unlock()
	})
}
