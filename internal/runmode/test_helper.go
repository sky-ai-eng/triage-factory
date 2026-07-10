package runmode

// TestT is the minimal slice of *testing.T / *testing.B that
// SetForTest needs. Defining it locally lets this file decline to
// import the standard-library "testing" package — keeping production
// builds of internal/runmode clean of the testing-package surface
// area. *testing.T and *testing.B both satisfy this interface
// implicitly (Go's structural typing), so callers pass them directly
// without an adapter.
type TestT interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
}

// SetForTest swaps the process mode for the duration of t and
// restores the previous (currentMode, initialized) pair via
// t.Cleanup. Access to package state goes through modeMu so this
// helper is data-race-free, but it is not safe for overlapping
// parallel tests: it mutates shared global state, so concurrent
// callers can still interfere logically. Tests that call SetForTest
// must avoid running in parallel with each other or otherwise
// serialize their use of the helper.
//
// Sets initialized=true so any subsequent Init call inside the test
// follows the production "already initialized" branches (idempotent
// on same mode, error on conflict). Tests that specifically want to
// exercise Init's first-call branch can save+restore initialized
// directly within the test body, since they're whitebox tests in
// package runmode.
//
// Lives in a non-_test.go file so consumers' test packages can call
// it — a _test.go file would only be visible to runmode_test. The
// local TestT interface (see above) keeps the testing-package import
// out of production builds.
func SetForTest(t TestT, m Mode) {
	t.Helper()
	if m != ModeLocal && m != ModeMulti {
		t.Fatalf("runmode.SetForTest: unknown mode %q", m)
	}
	modeMu.Lock()
	prevMode, prevInit := currentMode, initialized
	currentMode = m
	initialized = true
	modeMu.Unlock()
	t.Cleanup(func() {
		modeMu.Lock()
		currentMode = prevMode
		initialized = prevInit
		modeMu.Unlock()
	})
}

// SetRoleForTest swaps the process deployment role for the duration of t
// and restores the previous (currentRole, roleInitialized) pair via
// t.Cleanup. Sets roleInitialized=true so Role() returns the injected
// value rather than falling back to a live TF_ROLE parse. Same caveats as
// SetForTest — data-race-free but not safe for overlapping parallel tests
// that mutate the same global.
//
// Lives here (not a _test.go file) so consumers' test packages (internal/
// app's exclusion test, internal/db's migration-gate tests) can force a
// role without threading TF_ROLE through the environment.
func SetRoleForTest(t TestT, r DeployRole) {
	t.Helper()
	if r != RoleAll && r != RoleControl && r != RoleExecutor {
		t.Fatalf("runmode.SetRoleForTest: unknown role %q", r)
	}
	roleMu.Lock()
	prevRole, prevInit := currentRole, roleInitialized
	currentRole = r
	roleInitialized = true
	roleMu.Unlock()
	t.Cleanup(func() {
		roleMu.Lock()
		currentRole = prevRole
		roleInitialized = prevInit
		roleMu.Unlock()
	})
}

// SetOrgCreationEnabledForTest swaps the process org-creation toggle
// for the duration of t and restores the previous (orgCreationPrevented,
// orgCreationInitialized) pair via t.Cleanup. Same caveats as SetForTest
// — data-race-free but not safe for overlapping parallel tests that
// mutate the same global.
func SetOrgCreationEnabledForTest(t TestT, enabled bool) {
	t.Helper()
	orgCreationMu.Lock()
	prev, prevInit := orgCreationPrevented, orgCreationInitialized
	orgCreationPrevented = !enabled
	orgCreationInitialized = true
	orgCreationMu.Unlock()
	t.Cleanup(func() {
		orgCreationMu.Lock()
		orgCreationPrevented = prev
		orgCreationInitialized = prevInit
		orgCreationMu.Unlock()
	})
}

// SetClientIPPolicyForTest configures the trusted-proxy allowlist + client-IP
// capture toggle for the duration of t, restoring prior state via t.Cleanup.
// trustedCIDR uses the same syntax as TF_TRUSTED_PROXY_CIDR (comma-separated
// CIDRs/IPs, "none", or empty); capture maps to TF_CAPTURE_CLIENT_IP. Fatals
// on a parse error. Same caveats as SetForTest — data-race-free but not safe
// for overlapping parallel tests that mutate the same globals. Lives here (not
// a _test.go file) so internal/server tests can drive clientIP's policy.
func SetClientIPPolicyForTest(t TestT, trustedCIDR string, capture bool) {
	t.Helper()
	nets, none, err := ParseTrustedProxyCIDR(trustedCIDR)
	if err != nil {
		t.Fatalf("runmode.SetClientIPPolicyForTest: parse %q: %v", trustedCIDR, err)
	}
	clientIPMu.Lock()
	prevNets, prevCapture, prevConfigured := trustedProxies, captureClientIP, trustedProxyConfigured
	trustedProxies = nets
	captureClientIP = capture && !none
	trustedProxyConfigured = len(nets) > 0
	clientIPMu.Unlock()
	t.Cleanup(func() {
		clientIPMu.Lock()
		trustedProxies, captureClientIP, trustedProxyConfigured = prevNets, prevCapture, prevConfigured
		clientIPMu.Unlock()
	})
}
