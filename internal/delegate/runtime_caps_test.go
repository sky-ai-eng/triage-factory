package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Both engines are taught, and an engine nobody has taught lands on the zero
// value rather than on whichever answer the switch happened to fall through to.
// That is the property every call site's safe arm is chosen against — a boolean
// would have had to pick one polarity for all of them.
func TestRuntimeCaps_UnknownEngineIsItsOwnAnswer(t *testing.T) {
	for _, tc := range []struct {
		runtime string
		input   inputRole
		resume  resumeSource
	}{
		{domain.ConversationRuntimeNative, inputRoleLiveQueue, resumeSourceMessages},
		{domain.ConversationRuntimeSDK, inputRoleStagedResume, resumeSourceSession},
		{"", inputRoleUnknown, resumeSourceUnknown},
		{"an-engine-from-a-later-ticket", inputRoleUnknown, resumeSourceUnknown},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			if got := undeliveredRowsFor(tc.runtime); got != tc.input {
				t.Errorf("undeliveredRowsFor = %v, want %v", got, tc.input)
			}
			if got := resumeSourceFor(tc.runtime); got != tc.resume {
				t.Errorf("resumeSourceFor = %v, want %v", got, tc.resume)
			}
		})
	}
}

// The unknown answers are not merely distinct, they are the ones that make each
// caller's arm the conservative one: rows are left alone rather than consumed
// or re-routed, and neither resume affordance is extended.
func TestRuntimeCaps_UnknownEngineGrantsNothing(t *testing.T) {
	const later = "an-engine-from-a-later-ticket"
	if undeliveredRowsFor(later) == inputRoleStagedResume {
		t.Error("an untaught engine's rows would be consumed as staged resume input")
	}
	if resumeSourceFor(later) == resumeSourceMessages {
		t.Error("an untaught engine would be handed a fresh empty workspace as a continuation")
	}
	if drainsUndeliveredInput(domain.Conversation{Runtime: later, Status: domain.StatusQueued}) {
		t.Error("an untaught engine was credited with draining its own queue")
	}
}
