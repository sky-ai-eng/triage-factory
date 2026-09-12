package app

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestBuildMemoryProvisioner_EveryBrainCapableRoleInBothModes pins the one way
// this differs from the credential provisioner beside it: the gate is
// brain-capability alone, not brain-capability in multi mode. Local is always
// the brain, and a conversation that ends there owes a memory exactly as one in
// a fleet does — a nil provisioner at role=all would leave every local boundary
// owing forever, with the sweep that settles it never started.
func TestBuildMemoryProvisioner_EveryBrainCapableRoleInBothModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan subsystemPlan
		want bool
	}{
		{"local (role=all) is always the brain", subsystemPlan{role: runmode.RoleAll, brain: true}, true},
		{"a control pod is brain-capable", subsystemPlan{role: runmode.RoleControl, brain: true}, true},
		{"an executor never is", subsystemPlan{role: runmode.RoleExecutor}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{plan: tc.plan}
			a.buildMemoryProvisioner()
			if got := a.memoryProvisioner != nil; got != tc.want {
				t.Errorf("memoryProvisioner built = %v, want %v", got, tc.want)
			}
		})
	}
}
