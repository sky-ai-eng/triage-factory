package runmode

import "testing"

func TestParseRole(t *testing.T) {
	cases := []struct {
		in      string
		want    DeployRole
		wantErr bool
	}{
		{"", RoleAll, false},
		{"all", RoleAll, false},
		{"ALL", RoleAll, false},
		{"  all  ", RoleAll, false},
		{"control", RoleControl, false},
		{"Control", RoleControl, false},
		{"executor", RoleExecutor, false},
		{"EXECUTOR", RoleExecutor, false},
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

// TestInitRoleFromEnv_MultiHonorsRole pins that in multi mode the parsed
// role is installed verbatim — no coercion.
func TestInitRoleFromEnv_MultiHonorsRole(t *testing.T) {
	SetForTest(t, ModeMulti)
	// Reset role state so InitRole's first-call branch runs.
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "executor")

	coerced, requested, err := InitRoleFromEnv()
	if err != nil {
		t.Fatalf("InitRoleFromEnv: %v", err)
	}
	if coerced {
		t.Error("multi mode must not coerce a valid role")
	}
	if requested != RoleExecutor {
		t.Errorf("requested = %q, want executor", requested)
	}
	if Role() != RoleExecutor {
		t.Errorf("Role() = %q, want executor", Role())
	}
}

// TestInitRoleFromEnv_LocalForcesAll pins that any non-all role is coerced
// to all in local mode, with coerced=true so main() can warn.
func TestInitRoleFromEnv_LocalForcesAll(t *testing.T) {
	SetForTest(t, ModeLocal)
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "executor")

	coerced, requested, err := InitRoleFromEnv()
	if err != nil {
		t.Fatalf("InitRoleFromEnv: %v", err)
	}
	if !coerced {
		t.Error("local mode must coerce a non-all role to all (coerced=true)")
	}
	if requested != RoleExecutor {
		t.Errorf("requested = %q, want executor (the pre-coercion value)", requested)
	}
	if Role() != RoleAll {
		t.Errorf("Role() = %q, want all — local forces all", Role())
	}
}

// TestInitRoleFromEnv_InvalidFailsBoot pins that a typo'd role refuses to
// boot rather than defaulting to all.
func TestInitRoleFromEnv_InvalidFailsBoot(t *testing.T) {
	SetForTest(t, ModeMulti)
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "exectuor")

	if _, _, err := InitRoleFromEnv(); err == nil {
		t.Fatal("InitRoleFromEnv with an invalid TF_ROLE must return an error, not silently default to all")
	}
}

// TestInitRoleFromEnv_MultiRejectsAll pins the third boot rule: multi mode
// refuses to run as the fused single process, whether TF_ROLE says "all"
// explicitly or is simply unset (which parses to all). The error is the
// boot-time pointer at the split blueprint.
func TestInitRoleFromEnv_MultiRejectsAll(t *testing.T) {
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

// TestInitRole_MultiRejectsAll_DoesNotMutate pins that the rejection leaves
// the role uninitialized, so a subsequent valid init still works.
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

// TestInitRole_LocalStillAcceptsAll pins that the multi rejection does not
// leak into local mode: all (and unset) remain local's only shape.
func TestInitRole_LocalStillAcceptsAll(t *testing.T) {
	SetForTest(t, ModeLocal)
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "")

	coerced, _, err := InitRoleFromEnv()
	if err != nil {
		t.Fatalf("InitRoleFromEnv: %v", err)
	}
	if coerced {
		t.Error("unset TF_ROLE in local mode is not a coercion")
	}
	if Role() != RoleAll {
		t.Errorf("Role() = %q, want all", Role())
	}
}

// TestRole_UninitializedFallsBackToEnv pins the reset-hook behavior the
// internal/db migration-gate tests rely on: with the role never
// initialized, Role() honors a live TF_ROLE without the local-forces-all
// coupling (so an executor gate is reachable regardless of mode).
func TestRole_UninitializedFallsBackToEnv(t *testing.T) {
	resetRoleForTest(t)
	t.Setenv("TF_ROLE", "executor")
	if got := Role(); got != RoleExecutor {
		t.Errorf("uninitialized Role() = %q, want executor from live env", got)
	}
	t.Setenv("TF_ROLE", "garbage")
	if got := Role(); got != RoleAll {
		t.Errorf("uninitialized Role() with a bad env = %q, want all (safe fallback)", got)
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
