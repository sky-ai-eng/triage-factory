package runmode

import (
	"net"
	"strings"
	"testing"
)

func TestModeFromEnv(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeLocal, false},
		{"local", ModeLocal, false},
		{"Local", ModeLocal, false},
		{"LOCAL", ModeLocal, false},
		{"LoCaL", ModeLocal, false}, // arbitrary mixed case
		{"multi", ModeMulti, false},
		{"Multi", ModeMulti, false},
		{"MULTI", ModeMulti, false},
		{"MuLtI", ModeMulti, false},
		{"multi-tenant", "", true},
		{"prod", "", true},
		{" local ", "", true}, // exact match — no whitespace tolerance
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ModeFromEnv(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ModeFromEnv(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ModeFromEnv(%q) errored: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ModeFromEnv(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCurrent_DefaultsToLocal pins the package-init default. A test
// process that never calls Init or SetForTest must still see a usable
// mode — local is the safe choice because production behavior in
// local mode is what the test suite already expects.
func TestCurrent_DefaultsToLocal(t *testing.T) {
	// Don't call SetForTest here — we're explicitly checking the
	// init-time default. SetForTest's cleanup would mask any drift
	// in that default for subsequent tests.
	if got := Current(); got != ModeLocal {
		t.Errorf("Current() = %q at init time, want %q", got, ModeLocal)
	}
}

// withCleanInit clears the init flag for the duration of the test and
// restores the previous state on cleanup. Used by tests that exercise
// Init's first-call branch — without this, test-suite ordering would
// determine whether Init's been called by the time we run.
func withCleanInit(t *testing.T) {
	t.Helper()
	modeMu.Lock()
	prevMode, prevInit := currentMode, initialized
	currentMode = ModeLocal
	initialized = false
	modeMu.Unlock()
	t.Cleanup(func() {
		modeMu.Lock()
		currentMode = prevMode
		initialized = prevInit
		modeMu.Unlock()
	})
}

func TestInit_FirstCall(t *testing.T) {
	withCleanInit(t)
	if err := Init(ModeMulti); err != nil {
		t.Errorf("Init(ModeMulti) errored on clean slate: %v", err)
	}
	if got := Current(); got != ModeMulti {
		t.Errorf("after Init(ModeMulti), Current() = %q", got)
	}
}

func TestInit_IdempotentOnSameMode(t *testing.T) {
	withCleanInit(t)
	if err := Init(ModeLocal); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	if err := Init(ModeLocal); err != nil {
		t.Errorf("second Init with same mode should be a no-op, errored: %v", err)
	}
	if got := Current(); got != ModeLocal {
		t.Errorf("Current() = %q after double-Init(local), want %q", got, ModeLocal)
	}
}

func TestInit_RejectsConflictingReInit(t *testing.T) {
	withCleanInit(t)
	if err := Init(ModeLocal); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	err := Init(ModeMulti)
	if err == nil {
		t.Fatalf("second Init with different mode should have errored")
	}
	if !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("error should mention 'already initialized'; got %q", err.Error())
	}
	// Crucially: state must NOT have been mutated.
	if got := Current(); got != ModeLocal {
		t.Errorf("after rejected re-init, Current() = %q (must be unchanged)", got)
	}
}

func TestInit_RejectsUnknown(t *testing.T) {
	withCleanInit(t)
	err := Init(Mode("bogus"))
	if err == nil {
		t.Fatalf("Init(Mode(\"bogus\")) should have errored")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error message should reference the bad value; got %q", err.Error())
	}
	if got := Current(); got != ModeLocal {
		t.Errorf("after rejected Init, Current() = %q (must be unchanged)", got)
	}
}

// TestSetForTest_RestoresAfter exercises the t.Cleanup-based restore
// path. Subtest sets multi; after subtest exits, parent sees local
// again (because the parent's SetForTest set local).
func TestSetForTest_RestoresAfter(t *testing.T) {
	SetForTest(t, ModeLocal)
	if got := Current(); got != ModeLocal {
		t.Fatalf("setup: Current() = %q, want %q", got, ModeLocal)
	}

	t.Run("inner-flips-to-multi", func(t *testing.T) {
		SetForTest(t, ModeMulti)
		if got := Current(); got != ModeMulti {
			t.Errorf("inside subtest: Current() = %q, want %q", got, ModeMulti)
		}
	})

	if got := Current(); got != ModeLocal {
		t.Errorf("after subtest restore: Current() = %q, want %q", got, ModeLocal)
	}
}

// TestSetForTest_FlipsInitialized confirms SetForTest treats the test
// as "post-init", so a subsequent Init follows the conflict / idempotent
// branches rather than the first-call branch.
func TestSetForTest_FlipsInitialized(t *testing.T) {
	SetForTest(t, ModeLocal)
	// Init with the same mode is the idempotent case.
	if err := Init(ModeLocal); err != nil {
		t.Errorf("Init(ModeLocal) after SetForTest(local) should be idempotent, errored: %v", err)
	}
	// Init with a different mode is the conflict case.
	if err := Init(ModeMulti); err == nil {
		t.Errorf("Init(ModeMulti) after SetForTest(local) should error, got nil")
	}
}

// Org-creation toggle parsing + init contract mirror the mode tests.

func TestParsePreventOrgCreation(t *testing.T) {
	cases := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"false", false, false},
		{"0", false, false},
		{"no", false, false},
		{"off", false, false},
		{"true", true, false},
		{"1", true, false},
		{"yes", true, false},
		{"on", true, false},
		// Case-insensitive + whitespace-trimmed for the common bool spellings.
		{"TRUE", true, false},
		{"  true  ", true, false},
		// Anything that isn't a recognizable bool must fatal, not degrade.
		{"maybe", false, true},
		{"enabled", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParsePreventOrgCreation(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParsePreventOrgCreation(%q) = %t, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePreventOrgCreation(%q) errored: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParsePreventOrgCreation(%q) = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}

// withCleanOrgCreationInit mirrors withCleanInit for the org-creation
// state. Tests that exercise InitOrgCreationPrevented's first-call
// branch use this so package-init state doesn't leak across ordering.
func withCleanOrgCreationInit(t *testing.T) {
	t.Helper()
	orgCreationMu.Lock()
	prev, prevInit := orgCreationPrevented, orgCreationInitialized
	orgCreationPrevented = false
	orgCreationInitialized = false
	orgCreationMu.Unlock()
	t.Cleanup(func() {
		orgCreationMu.Lock()
		orgCreationPrevented = prev
		orgCreationInitialized = prevInit
		orgCreationMu.Unlock()
	})
}

func TestOrgCreation_DefaultsToEnabled(t *testing.T) {
	withCleanOrgCreationInit(t)
	if !OrgCreationEnabled() {
		t.Errorf("OrgCreationEnabled() = false at init time, want true (permissive default)")
	}
}

func TestInitOrgCreationPrevented_FirstCall(t *testing.T) {
	withCleanOrgCreationInit(t)
	if err := InitOrgCreationPrevented(true); err != nil {
		t.Errorf("InitOrgCreationPrevented(true) errored on clean slate: %v", err)
	}
	if OrgCreationEnabled() {
		t.Errorf("after InitOrgCreationPrevented(true), OrgCreationEnabled() = true")
	}
}

func TestInitOrgCreationPrevented_IdempotentOnSame(t *testing.T) {
	withCleanOrgCreationInit(t)
	if err := InitOrgCreationPrevented(true); err != nil {
		t.Fatalf("first InitOrgCreationPrevented: %v", err)
	}
	if err := InitOrgCreationPrevented(true); err != nil {
		t.Errorf("second InitOrgCreationPrevented with same value should be a no-op, errored: %v", err)
	}
}

func TestInitOrgCreationPrevented_RejectsConflictingReInit(t *testing.T) {
	withCleanOrgCreationInit(t)
	if err := InitOrgCreationPrevented(false); err != nil {
		t.Fatalf("first InitOrgCreationPrevented: %v", err)
	}
	err := InitOrgCreationPrevented(true)
	if err == nil {
		t.Fatalf("conflicting InitOrgCreationPrevented should have errored")
	}
	if !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("error should mention 'already initialized'; got %q", err.Error())
	}
	if !OrgCreationEnabled() {
		t.Errorf("after rejected re-init, OrgCreationEnabled() = false (must be unchanged)")
	}
}

func TestSetOrgCreationEnabledForTest_RestoresAfter(t *testing.T) {
	SetOrgCreationEnabledForTest(t, true)
	if !OrgCreationEnabled() {
		t.Fatalf("setup: OrgCreationEnabled() = false, want true")
	}
	t.Run("inner-flips-to-prevented", func(t *testing.T) {
		SetOrgCreationEnabledForTest(t, false)
		if OrgCreationEnabled() {
			t.Errorf("inside subtest: OrgCreationEnabled() = true, want false")
		}
	})
	if !OrgCreationEnabled() {
		t.Errorf("after subtest restore: OrgCreationEnabled() = false, want true")
	}
}

func TestParseTrustedProxyCIDR(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantCount int  // number of parsed CIDRs
		wantNone  bool // the "none" capture kill switch
		wantErr   bool
		// contains/excludes probe Contains() on the parsed set (only when
		// wantCount > 0), proving the masks landed right.
		contains []string
		excludes []string
	}{
		{name: "empty", in: "", wantCount: 0},
		{name: "whitespace only", in: "   ", wantCount: 0},
		{name: "none sentinel", in: "none", wantNone: true},
		{name: "none case-insensitive", in: "  NoNe ", wantNone: true},
		{
			name: "single cidr v4", in: "10.0.0.0/8", wantCount: 1,
			contains: []string{"10.1.2.3"}, excludes: []string{"11.0.0.1"},
		},
		{
			name: "bare ip promoted to /32", in: "203.0.113.7", wantCount: 1,
			contains: []string{"203.0.113.7"}, excludes: []string{"203.0.113.8"},
		},
		{
			name: "ipv6 cidr", in: "2001:db8::/32", wantCount: 1,
			contains: []string{"2001:db8::1"}, excludes: []string{"2001:dead::1"},
		},
		{
			name: "bare ipv6 promoted to /128", in: "2001:db8::1", wantCount: 1,
			contains: []string{"2001:db8::1"}, excludes: []string{"2001:db8::2"},
		},
		{
			name: "multiple mixed with spaces", in: "10.0.0.0/8, 192.168.0.0/16 , 203.0.113.7",
			wantCount: 3, contains: []string{"10.9.9.9", "192.168.1.1", "203.0.113.7"},
			excludes: []string{"8.8.8.8"},
		},
		{name: "empty entries skipped", in: "10.0.0.0/8,,", wantCount: 1},
		// A typo must fail loudly, not silently drop to the wrong default.
		{name: "garbage errors", in: "not-an-ip", wantErr: true},
		{name: "bad mask errors", in: "10.0.0.0/99", wantErr: true},
		{name: "none mixed with cidr errors", in: "none,10.0.0.0/8", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nets, none, err := ParseTrustedProxyCIDR(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTrustedProxyCIDR(%q) = %v, nil; want error", tc.in, nets)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTrustedProxyCIDR(%q) errored: %v", tc.in, err)
			}
			if none != tc.wantNone {
				t.Errorf("none = %v, want %v", none, tc.wantNone)
			}
			if len(nets) != tc.wantCount {
				t.Fatalf("parsed %d CIDRs, want %d", len(nets), tc.wantCount)
			}
			for _, probe := range tc.contains {
				if !ipInSet(nets, probe) {
					t.Errorf("%q should be contained in the parsed set", probe)
				}
			}
			for _, probe := range tc.excludes {
				if ipInSet(nets, probe) {
					t.Errorf("%q should NOT be contained in the parsed set", probe)
				}
			}
		})
	}
}

// ipInSet is a test helper mirroring the membership check clientIP makes.
func ipInSet(nets []*net.IPNet, ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func TestParseCaptureClientIP(t *testing.T) {
	cases := []struct {
		in      string
		want    bool
		wantErr bool
	}{
		// Empty defaults to true (capture on) — the inverse of
		// ParsePreventOrgCreation's default.
		{"", true, false},
		{"true", true, false},
		{"1", true, false},
		{"on", true, false},
		{"yes", true, false},
		{"false", false, false},
		{"0", false, false},
		{"off", false, false},
		{"no", false, false},
		{"  FALSE  ", false, false}, // case-insensitive + trimmed
		{"maybe", false, true},
		{"enabled", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseCaptureClientIP(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseCaptureClientIP(%q) = %t, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCaptureClientIP(%q) errored: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseCaptureClientIP(%q) = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}

func TestInitClientIPPolicyFromEnv(t *testing.T) {
	t.Run("unset → capture on, not configured", func(t *testing.T) {
		t.Setenv("TF_TRUSTED_PROXY_CIDR", "")
		t.Setenv("TF_CAPTURE_CLIENT_IP", "")
		restoreClientIPPolicy(t)
		if err := InitClientIPPolicyFromEnv(); err != nil {
			t.Fatalf("InitClientIPPolicyFromEnv: %v", err)
		}
		if !CaptureClientIP() {
			t.Errorf("CaptureClientIP() = false, want true (default)")
		}
		if TrustedProxyConfigured() {
			t.Errorf("TrustedProxyConfigured() = true, want false (unset)")
		}
		if len(TrustedProxies()) != 0 {
			t.Errorf("TrustedProxies() = %v, want empty", TrustedProxies())
		}
	})

	t.Run("cidr set → configured", func(t *testing.T) {
		t.Setenv("TF_TRUSTED_PROXY_CIDR", "10.0.0.0/8")
		t.Setenv("TF_CAPTURE_CLIENT_IP", "")
		restoreClientIPPolicy(t)
		if err := InitClientIPPolicyFromEnv(); err != nil {
			t.Fatalf("InitClientIPPolicyFromEnv: %v", err)
		}
		if !TrustedProxyConfigured() {
			t.Errorf("TrustedProxyConfigured() = false, want true")
		}
		if !CaptureClientIP() {
			t.Errorf("CaptureClientIP() = false, want true")
		}
		if !ipInSet(TrustedProxies(), "10.1.1.1") {
			t.Errorf("10.1.1.1 should be in the trusted set")
		}
	})

	t.Run("capture=false → off", func(t *testing.T) {
		t.Setenv("TF_TRUSTED_PROXY_CIDR", "10.0.0.0/8")
		t.Setenv("TF_CAPTURE_CLIENT_IP", "false")
		restoreClientIPPolicy(t)
		if err := InitClientIPPolicyFromEnv(); err != nil {
			t.Fatalf("InitClientIPPolicyFromEnv: %v", err)
		}
		if CaptureClientIP() {
			t.Errorf("CaptureClientIP() = true, want false")
		}
	})

	t.Run("none sentinel → capture off even when toggle unset", func(t *testing.T) {
		t.Setenv("TF_TRUSTED_PROXY_CIDR", "none")
		t.Setenv("TF_CAPTURE_CLIENT_IP", "")
		restoreClientIPPolicy(t)
		if err := InitClientIPPolicyFromEnv(); err != nil {
			t.Fatalf("InitClientIPPolicyFromEnv: %v", err)
		}
		if CaptureClientIP() {
			t.Errorf("CaptureClientIP() = true, want false (none sentinel)")
		}
		if TrustedProxyConfigured() {
			t.Errorf("TrustedProxyConfigured() = true, want false")
		}
	})

	t.Run("malformed cidr errors", func(t *testing.T) {
		t.Setenv("TF_TRUSTED_PROXY_CIDR", "bogus")
		t.Setenv("TF_CAPTURE_CLIENT_IP", "")
		restoreClientIPPolicy(t)
		if err := InitClientIPPolicyFromEnv(); err == nil {
			t.Fatalf("InitClientIPPolicyFromEnv() = nil, want error on malformed CIDR")
		}
	})

	t.Run("malformed toggle errors", func(t *testing.T) {
		t.Setenv("TF_TRUSTED_PROXY_CIDR", "")
		t.Setenv("TF_CAPTURE_CLIENT_IP", "maybe")
		restoreClientIPPolicy(t)
		if err := InitClientIPPolicyFromEnv(); err == nil {
			t.Fatalf("InitClientIPPolicyFromEnv() = nil, want error on malformed toggle")
		}
	})
}

// restoreClientIPPolicy snapshots the client-IP policy globals and restores
// them after the test, so InitClientIPPolicyFromEnv calls don't leak across
// cases (the production setter has no idempotency guard by design — one boot
// caller).
func restoreClientIPPolicy(t *testing.T) {
	t.Helper()
	clientIPMu.Lock()
	prevNets, prevCapture, prevConfigured := trustedProxies, captureClientIP, trustedProxyConfigured
	clientIPMu.Unlock()
	t.Cleanup(func() {
		clientIPMu.Lock()
		trustedProxies, captureClientIP, trustedProxyConfigured = prevNets, prevCapture, prevConfigured
		clientIPMu.Unlock()
	})
}

func TestSetClientIPPolicyForTest_RestoresAfter(t *testing.T) {
	SetClientIPPolicyForTest(t, "10.0.0.0/8", true)
	if !CaptureClientIP() || !TrustedProxyConfigured() {
		t.Fatalf("setup: capture=%v configured=%v, want true/true", CaptureClientIP(), TrustedProxyConfigured())
	}
	t.Run("inner-flips-to-capture-off", func(t *testing.T) {
		SetClientIPPolicyForTest(t, "", false)
		if CaptureClientIP() {
			t.Errorf("inside subtest: CaptureClientIP() = true, want false")
		}
		if TrustedProxyConfigured() {
			t.Errorf("inside subtest: TrustedProxyConfigured() = true, want false")
		}
	})
	if !CaptureClientIP() || !TrustedProxyConfigured() {
		t.Errorf("after subtest restore: capture=%v configured=%v, want true/true", CaptureClientIP(), TrustedProxyConfigured())
	}
}
