package domain

import "testing"

func TestRunStatusClassification(t *testing.T) {
	for _, s := range AllTerminalRunStatuses() {
		if !IsTerminalRunStatus(s) {
			t.Errorf("%q should be terminal", s)
		}
		if IsActiveRunStatus(s) {
			t.Errorf("%q is terminal, must not be active", s)
		}
		if IsClaimPhase(s) {
			t.Errorf("%q is a terminal status, not a claim phase", s)
		}
	}

	// In-flight (claimed, setting up or executing) → active. Every claim
	// phase counts, and so does running: a conversation setting up occupies
	// an executor slot exactly like one calling the model.
	for _, s := range append(AllClaimPhases(), StatusRunning) {
		if IsTerminalRunStatus(s) {
			t.Errorf("%q should not be terminal", s)
		}
		if !IsActiveRunStatus(s) {
			t.Errorf("%q should be active", s)
		}
	}

	// Neither active nor terminal: queued (not yet claimed) and open (parked).
	for _, s := range []string{StatusQueued, StatusOpen} {
		if IsTerminalRunStatus(s) {
			t.Errorf("%q should not be terminal", s)
		}
		if IsActiveRunStatus(s) {
			t.Errorf("%q should NOT be active (queued/parked)", s)
		}
	}
}

// The classifier is closed-world: it names what active IS. Anything outside
// the vocabulary — a typo, a status this build predates, or a raw NULL that
// escaped the display ladder — must classify as not-active rather than
// silently counting as a live executor slot.
func TestIsActiveRunStatus_ClosedWorld(t *testing.T) {
	for _, s := range []string{
		"",
		"initializing",     // never existed backend-side
		"worktree_created", // ditto
		"pending_approval", // removed: approval is a derived artifact view
		"RUNNING",          // case matters
		"fetchin",          // typo
	} {
		if IsActiveRunStatus(s) {
			t.Errorf("IsActiveRunStatus(%q) = true, want false — unrecognized values are not active", s)
		}
		if IsClaimPhase(s) {
			t.Errorf("IsClaimPhase(%q) = true, want false", s)
		}
	}
}

// AllRunStatuses is the union the frontend mirror is checked against, so it
// must stay exactly the three non-phase non-terminal names plus both sets,
// with no duplicates.
func TestAllRunStatuses_IsTheWholeVocabulary(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range AllRunStatuses() {
		if seen[s] {
			t.Errorf("AllRunStatuses lists %q twice", s)
		}
		seen[s] = true
	}
	want := append(AllClaimPhases(), AllTerminalRunStatuses()...)
	want = append(want, StatusQueued, StatusRunning, StatusOpen)
	for _, s := range want {
		if !seen[s] {
			t.Errorf("AllRunStatuses is missing %q", s)
		}
	}
	if len(AllRunStatuses()) != len(want) {
		t.Errorf("AllRunStatuses has %d entries, want %d", len(AllRunStatuses()), len(want))
	}
}

// Callers append to the returned slices (the classification test above does).
// Each call must hand back a fresh backing array, or one caller's append would
// scribble on the next caller's view of the vocabulary.
func TestVocabularyAccessorsReturnFreshSlices(t *testing.T) {
	first := AllClaimPhases()
	_ = append(AllClaimPhases(), "sentinel")
	got := AllClaimPhases()
	if len(got) != len(first) {
		t.Fatalf("AllClaimPhases mutated across calls: %v", got)
	}
	for i, s := range got {
		if s != first[i] {
			t.Errorf("AllClaimPhases[%d] = %q, want %q", i, s, first[i])
		}
	}
}

// The retired terminals are a two-vocabulary problem, and getting either half
// wrong is a real bug in a different direction. Classified as terminal but
// absent from the written set: a guard that forgot them would readmit a
// long-finished run to parking or the active-work counters, while a UI arm for
// a state nothing emits is dead code that outlives its backend (exactly how
// initializing / worktree_created / pending_approval survived).
func TestRetiredTerminalsAreTerminalButNotWritten(t *testing.T) {
	for _, s := range []string{StatusRetiredCancelled, StatusRetiredTaskUnsolvable} {
		if !IsTerminalRunStatus(s) {
			t.Errorf("%q must classify as terminal — the rows exist and every guard over them is an exclusion", s)
		}
		if IsActiveRunStatus(s) {
			t.Errorf("%q is terminal, must not be active", s)
		}
		for _, written := range AllTerminalRunStatuses() {
			if s == written {
				t.Errorf("%q is retired and must not appear in the written vocabulary the frontend mirrors", s)
			}
		}
		for _, displayed := range AllRunStatuses() {
			if s == displayed {
				t.Errorf("%q is retired and must not appear in AllRunStatuses", s)
			}
		}
	}
	// The stored set is the union, and it is what SQL guards mirror.
	stored := map[string]bool{}
	for _, s := range AllStoredTerminalRunStatuses() {
		stored[s] = true
	}
	for _, s := range append(AllTerminalRunStatuses(), StatusRetiredCancelled, StatusRetiredTaskUnsolvable) {
		if !stored[s] {
			t.Errorf("AllStoredTerminalRunStatuses is missing %q", s)
		}
	}
	if len(AllStoredTerminalRunStatuses()) != len(stored) {
		t.Error("AllStoredTerminalRunStatuses has duplicates")
	}
}
