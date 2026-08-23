package app

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestPlanForRole_Positive pins the positive shape of each role's subsystem
// inventory, so a wiring change that flips a group for the wrong role fails
// here.
func TestPlanForRole_Positive(t *testing.T) {
	cases := []struct {
		role                                      runmode.DeployRole
		serveHTTP, brain, dispatcher, execHealthz bool
	}{
		{runmode.RoleAll, true, true, true, false},
		{runmode.RoleControl, true, true, false, false},
		{runmode.RoleExecutor, false, false, true, true},
	}
	for _, tc := range cases {
		p := planForRole(tc.role)
		if p.serveHTTP != tc.serveHTTP {
			t.Errorf("%s: serveHTTP = %v, want %v", tc.role, p.serveHTTP, tc.serveHTTP)
		}
		if p.brain != tc.brain {
			t.Errorf("%s: brain = %v, want %v", tc.role, p.brain, tc.brain)
		}
		if p.dispatcher != tc.dispatcher {
			t.Errorf("%s: dispatcher = %v, want %v", tc.role, p.dispatcher, tc.dispatcher)
		}
		if p.executorHealthz != tc.execHealthz {
			t.Errorf("%s: executorHealthz = %v, want %v", tc.role, p.executorHealthz, tc.execHealthz)
		}
	}
}

// TestExecutorPlan_ExcludesEveryBrainAndHTTPSubsystem is the exclusion
// guard the ticket mandates: rather than hand-list what an executor starts,
// it asserts what the executor must NOT start. Each forbidden subsystem is
// mapped to the plan group that gates it in the wiring — so a NEW subsystem
// added to app.Run later must be gated on serveHTTP or brain (defaulting it
// onto control), and if someone instead leaks it onto executors (gating on
// dispatcher or nothing) this test is where the omission surfaces.
func TestExecutorPlan_ExcludesEveryBrainAndHTTPSubsystem(t *testing.T) {
	p := planForRole(runmode.RoleExecutor)

	// Forbidden-on-executor subsystems and the plan group each is gated on.
	// If the group is true for an executor, the subsystem would start there.
	forbidden := []struct {
		name    string
		started bool
	}{
		{"HTTP/API server", p.serveHTTP},
		{"websocket hub (user-facing)", p.serveHTTP},
		{"server extension workers", p.serveHTTP},
		{"dashboard backfiller", p.serveHTTP},
		{"pollers + tracker", p.brain},
		{"App-installation grant mirror reconcile", p.brain},
		{"event router + drain workers", p.brain},
		{"AI scorer manager", p.brain},
		{"repo-profiler manager", p.brain},
		{"artifact reconciler manager", p.brain},
		{"reachable-repo cache manager", p.brain},
		{"marketplace-stats manager", p.brain},
		{"poll-completion bus subscribers", p.brain},
	}
	for _, f := range forbidden {
		if f.started {
			t.Errorf("executor must NOT start %q, but its plan group is enabled", f.name)
		}
	}

	// And it MUST start exactly the sandbox-worker set.
	if !p.dispatcher {
		t.Error("executor must start the conversation-queue dispatcher")
	}
	if !p.executorHealthz {
		t.Error("executor must start the localhost healthz listener")
	}
}

// TestControlPlan_NoDispatcher pins the other half of the split: a control
// pod serves HTTP and runs the brain but never claims delegated runs.
func TestControlPlan_NoDispatcher(t *testing.T) {
	p := planForRole(runmode.RoleControl)
	if p.dispatcher {
		t.Error("control must NOT run the conversation-queue dispatcher")
	}
	if p.executorHealthz {
		t.Error("control exposes /api/health + /readyz on the main port; no separate executor healthz")
	}
	if !p.serveHTTP || !p.brain {
		t.Error("control must serve HTTP and run the brain")
	}
}
