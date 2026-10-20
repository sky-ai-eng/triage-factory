package agentproc

import "testing"

// Close is idempotent, as cell.Jail requires: the live-jail entry is released
// once, so a second Close cannot drop a successor's entry under the same
// container id.
func TestToolHostJail_CloseIsIdempotent(t *testing.T) {
	releases := 0
	jail := &ToolHostJail{releaseLiveJail: func() { releases++ }}
	_ = jail.Close()
	_ = jail.Close()
	if releases != 1 {
		t.Fatalf("Close released the live-jail entry %d times, want 1", releases)
	}

	var none *ToolHostJail
	if !none.Exited() {
		t.Error("a nil jail reports processes still running")
	}
}
