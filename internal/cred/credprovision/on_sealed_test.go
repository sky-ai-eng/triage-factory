package credprovision

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventsource"
)

// TestProvision_TellsOfTheSealsAWaitIsWaitingOn: a seal answering an
// executor's request, or the awaiting sweep's seal of a parked claim, is told
// once its bundle is written, naming the conversation; a refresh sweep's
// re-seal of a running engagement is not, since nothing waits on it.
func TestProvision_TellsOfTheSealsAWaitIsWaitingOn(t *testing.T) {
	for _, tc := range []struct {
		reason   Reason
		wantTold bool
	}{
		{ReasonCredRequest, true},
		{ReasonAwaitingSweep, true},
		{ReasonRefreshSweep, false},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			rig := newProvisionRig(t, eventsource.Availability{
				eventsource.NewSource(eventsource.KindGitHub, eventsource.StateAvailable),
			}, &installTokens{})
			type told struct{ orgID, conversationID string }
			var got []told
			rig.m.SetOnSealed(func(_ context.Context, orgID, conversationID string) {
				if rig.claims.puts == 0 {
					t.Error("told of the seal before its bundle was written")
				}
				got = append(got, told{orgID, conversationID})
			})

			if err := rig.m.ProvisionForConversation(context.Background(), "org-1", "conv-1", tc.reason); err != nil {
				t.Fatalf("ProvisionForConversation: %v", err)
			}
			switch {
			case !tc.wantTold && len(got) != 0:
				t.Errorf("told %+v of a %s seal, want nothing", got, tc.reason)
			case tc.wantTold && (len(got) != 1 || got[0] != told{"org-1", "conv-1"}):
				t.Errorf("told %+v of a %s seal, want exactly org-1/conv-1", got, tc.reason)
			}
		})
	}
}
