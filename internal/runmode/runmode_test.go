package runmode

import (
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
