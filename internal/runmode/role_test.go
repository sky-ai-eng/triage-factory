package runmode

import "testing"

func TestParseRole(t *testing.T) {
	cases := []struct {
		in      string
		want    DeployRole
		wantErr bool
	}{
		{"control", RoleControl, false},
		{"Control", RoleControl, false},
		{"executor", RoleExecutor, false},
		{"EXECUTOR", RoleExecutor, false},
		{"  control  ", RoleControl, false},
		{"", "", true},    // unset is an error — multi requires an explicit split role
		{"all", "", true}, // not a deployable role: local ignores TF_ROLE, multi splits
		{"ALL", "", true},
		{"exectuor", "", true}, // the ticket's canonical typo
		{"worker", "", true},
		{" multi ", "", true},
	}
	for _, tc := range cases {
		got, err := ParseRole(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseRole(%q) err = nil, want an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRole(%q) err = %v, want nil", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRole(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInitRoleFromEnv_MultiHonorsRole pins that in multi mode a split role
// is installed verbatim.
func TestInitRoleFromEnv_MultiHonorsRole(t *testing.T) {
	SetForTest(t, ModeMulti)
	// Reset role state so InitRole's first-call branch runs.
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "executor")

	ignored, _, err := InitRoleFromEnv()
	if err != nil {
		t.Fatalf("InitRoleFromEnv: %v", err)
	}
	if ignored {
		t.Error("multi mode must never report TF_ROLE as ignored")
	}
	if Role() != RoleExecutor {
		t.Errorf("Role() = %q, want executor", Role())
	}
}

// TestInitRoleFromEnv_LocalIgnoresRole pins that local mode ignores TF_ROLE
// outright — the single-process shape is gated on the mode, not a role. Any
// set value (a split role, the retired "all", garbage) resolves to the
// single-process shape with ignored=true so main() can warn; a stray env
// var never bricks a laptop.
func TestInitRoleFromEnv_LocalIgnoresRole(t *testing.T) {
	for _, val := range []string{"executor", "control", "all", "garbage-not-a-role"} {
		t.Run("TF_ROLE="+val, func(t *testing.T) {
			SetForTest(t, ModeLocal)
			resetRoleForTest(t)
			t.Setenv("TF_ROLE", val)

			ignored, raw, err := InitRoleFromEnv()
			if err != nil {
				t.Fatalf("InitRoleFromEnv: %v (local must never fail on TF_ROLE)", err)
			}
			if !ignored {
				t.Error("a set TF_ROLE in local mode must report ignored=true so main() warns")
			}
			if raw != val {
				t.Errorf("rawValue = %q, want %q (the warning names the ignored value)", raw, val)
			}
			if Role() != RoleAll {
				t.Errorf("Role() = %q, want the single-process shape", Role())
			}
		})
	}
}

// TestInitRoleFromEnv_LocalUnsetIsSilent pins that the common case — local
// with no TF_ROLE at all — resolves silently (no warning flag).
func TestInitRoleFromEnv_LocalUnsetIsSilent(t *testing.T) {
	SetForTest(t, ModeLocal)
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "")

	ignored, _, err := InitRoleFromEnv()
	if err != nil {
		t.Fatalf("InitRoleFromEnv: %v", err)
	}
	if ignored {
		t.Error("unset TF_ROLE in local mode is not a warning condition")
	}
	if Role() != RoleAll {
		t.Errorf("Role() = %q, want the single-process shape", Role())
	}
}

// TestInitRoleFromEnv_InvalidFailsBoot pins that a typo'd role refuses to
// boot in multi mode rather than defaulting to anything.
func TestInitRoleFromEnv_InvalidFailsBoot(t *testing.T) {
	SetForTest(t, ModeMulti)
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "exectuor")

	if _, _, err := InitRoleFromEnv(); err == nil {
		t.Fatal("InitRoleFromEnv with an invalid TF_ROLE must return an error in multi mode")
	}
}

// TestInitRoleFromEnv_MultiRejectsAllAndUnset pins the closed topology:
// multi mode refuses to run as a fused single process, whether TF_ROLE says
// "all" explicitly or is simply unset. The error is the boot-time pointer
// at the split blueprint.
func TestInitRoleFromEnv_MultiRejectsAllAndUnset(t *testing.T) {
	for _, val := range []string{"", "all", "ALL"} {
		t.Run("TF_ROLE="+val, func(t *testing.T) {
			SetForTest(t, ModeMulti)
			resetRoleForTest(t)
			t.Setenv("TF_ROLE", val)

			if _, _, err := InitRoleFromEnv(); err == nil {
				t.Fatalf("InitRoleFromEnv with TF_ROLE=%q in multi mode must fail boot — multi is always the control+executor split", val)
			}
		})
	}
}

// TestInitRole_MultiRejectsAll_DoesNotMutate pins that the programmatic
// rejection leaves the role uninitialized, so a subsequent valid init still
// works.
func TestInitRole_MultiRejectsAll_DoesNotMutate(t *testing.T) {
	SetForTest(t, ModeMulti)
	resetRoleForTest(t)

	if err := InitRole(RoleAll); err == nil {
		t.Fatal("InitRole(all) in multi mode must error")
	}
	if err := InitRole(RoleControl); err != nil {
		t.Fatalf("InitRole(control) after a rejected all: %v", err)
	}
	if Role() != RoleControl {
		t.Errorf("Role() = %q, want control", Role())
	}
}

// TestRole_UninitializedFallsBackToEnv pins the reset-hook behavior the
// internal/db migration-gate tests rely on: with the role never
// initialized, Role() honors a live TF_ROLE without the boot-time mode
// policies (so an executor gate is reachable regardless of mode), and any
// unparseable or unset value degrades to the single-process shape.
func TestRole_UninitializedFallsBackToEnv(t *testing.T) {
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "executor")
	if got := Role(); got != RoleExecutor {
		t.Errorf("uninitialized Role() = %q, want executor from live env", got)
	}
	t.Setenv("TF_ROLE", "garbage")
	if got := Role(); got != RoleAll {
		t.Errorf("uninitialized Role() with a bad env = %q, want the single-process fallback", got)
	}
	t.Setenv("TF_ROLE", "")
	if got := Role(); got != RoleAll {
		t.Errorf("uninitialized Role() with no env = %q, want the single-process fallback", got)
	}
}

// TestInitRole_IdempotencyAndConflict pins the same first-call / same /
// conflict contract Init has.
func TestInitRole_IdempotencyAndConflict(t *testing.T) {
	SetForTest(t, ModeMulti)
	resetRoleForTest(t)

	if err := InitRole(RoleControl); err != nil {
		t.Fatalf("first InitRole: %v", err)
	}
	if err := InitRole(RoleControl); err != nil {
		t.Errorf("same-role re-init should be a no-op, got %v", err)
	}
	if err := InitRole(RoleExecutor); err == nil {
		t.Error("conflicting re-init should error")
	}
	if Role() != RoleControl {
		t.Errorf("Role() = %q, want control (conflict must not mutate)", Role())
	}
}

// resetRoleForTest clears the role init state so a test can exercise
// InitRole's first-call branch. Restores prior state on cleanup.
func resetRoleForTest(t *testing.T) {
	t.Helper()
	roleMu.Lock()
	prevRole, prevInit := currentRole, roleInitialized
	currentRole = RoleAll
	roleInitialized = false
	roleMu.Unlock()
	t.Cleanup(func() {
		roleMu.Lock()
		currentRole = prevRole
		roleInitialized = prevInit
		roleMu.Unlock()
	})
}
