package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestLoopRunsInJail_DefaultsToWithholdingTheChannel pins the direction of the
// only decision in the cell's provisioning that is a capability rather than a
// fact: which runtimes get a model-provider channel inside the jail.
//
// The two named engines are the easy half, and they would pass under either
// spelling. The case that matters is a value neither name covers — a later
// engine, or a row whose runtime never got written. "Anything but native"
// grants those a provider channel; naming the engine that needs one withholds
// it. Both are wrong for a runtime that genuinely runs its loop in the jail,
// and only one of them is wrong quietly.
func TestLoopRunsInJail_DefaultsToWithholdingTheChannel(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		want    bool
	}{
		{domain.ConversationRuntimeSDK, true},
		{domain.ConversationRuntimeNative, false},
		{"", false},
		{"some-later-engine", false},
	} {
		if got := loopRunsInJail(tc.runtime); got != tc.want {
			t.Errorf("loopRunsInJail(%q) = %v, want %v", tc.runtime, got, tc.want)
		}
	}
}
