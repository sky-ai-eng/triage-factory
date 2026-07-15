package domain

import "testing"

func TestRunStatusClassification(t *testing.T) {
	terminal := []string{"completed", "failed", "cancelled", "task_unsolvable"}
	for _, s := range terminal {
		if !IsTerminalRunStatus(s) {
			t.Errorf("%q should be terminal", s)
		}
		if IsActiveRunStatus(s) {
			t.Errorf("%q is terminal, must not be active", s)
		}
	}

	// In-flight (claimed, setting up or executing) → active.
	active := []string{"initializing", "cloning", "fetching", "worktree_created", "agent_starting", "running", "awaiting_credentials"}
	for _, s := range active {
		if IsTerminalRunStatus(s) {
			t.Errorf("%q should not be terminal", s)
		}
		if !IsActiveRunStatus(s) {
			t.Errorf("%q should be active", s)
		}
	}

	// Neither active nor terminal: queued (not yet claimed) and open (parked).
	for _, s := range []string{"queued", "open"} {
		if IsTerminalRunStatus(s) {
			t.Errorf("%q should not be terminal", s)
		}
		if IsActiveRunStatus(s) {
			t.Errorf("%q should NOT be active (queued/parked)", s)
		}
	}
}
