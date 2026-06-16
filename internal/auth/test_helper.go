package auth

// TestT is the minimal slice of *testing.T this package's cross-package test
// helpers need — mirrors runmode.TestT so production builds of internal/auth
// stay free of the standard-library "testing" import. *testing.T satisfies it
// structurally, so callers pass it directly.
type TestT interface {
	Helper()
	Cleanup(func())
}

// SetAnthropicModelsURLForTest points ValidateAnthropicAPIKey at a stub host
// for the duration of t and restores the production URL on cleanup, so tests
// in other packages (e.g. the /api/anthropic/connect handler) can exercise the
// validator against an httptest server. Test-only — production never reassigns
// the URL. Mutates a package global, so callers must not run in parallel with
// each other.
func SetAnthropicModelsURLForTest(t TestT, url string) {
	t.Helper()
	prev := anthropicModelsURL
	anthropicModelsURL = url
	t.Cleanup(func() { anthropicModelsURL = prev })
}
