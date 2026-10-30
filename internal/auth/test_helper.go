package auth

import "net/http"

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

// SetJiraTransportForTest sends ValidateJira's requests through rt for the
// duration of t and restores the client's transport on cleanup. A Cloud OAuth
// probe goes to the fixed api.atlassian.com gateway, so a test in another
// package answers it from a stub by rewriting the request here rather than by
// passing a URL. Mutates a package global, so callers must not run in parallel
// with each other.
func SetJiraTransportForTest(t TestT, rt http.RoundTripper) {
	t.Helper()
	prev := jiraHTTPClient.Transport
	jiraHTTPClient.Transport = rt
	t.Cleanup(func() { jiraHTTPClient.Transport = prev })
}

// ResetSecretBackendForTest clears the process-wide cached secret backend so the
// next secret call re-resolves it from the current environment (TF_SECRETS_BACKEND
// / TF_SECRET_ENCRYPTION_KEY / the keychain probe). The cache is global, so a test
// that forces the file backend must call this to avoid leaking that choice into
// later tests that expect the keychain — the t.Cleanup re-clears it on the way
// out. Test-only; mutates a package global, so callers must not run in parallel
// with each other.
func ResetSecretBackendForTest(t TestT) {
	t.Helper()
	backendMu.Lock()
	activeBackend = nil
	backendMu.Unlock()
	t.Cleanup(func() {
		backendMu.Lock()
		activeBackend = nil
		backendMu.Unlock()
	})
}
