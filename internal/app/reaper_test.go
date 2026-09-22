package app

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// TestBuildReaper_ClaimLeaseOrdering pins the boot refusal. A holder that
// cannot renew must have killed its own cell before the lease it can no
// longer prove lapses and a successor may take the conversation over; a
// configuration where the fence lands at or after the expiry cannot guarantee
// that, and there is no safe way to run it.
func TestBuildReaper_ClaimLeaseOrdering(t *testing.T) {
	a := &App{spawner: delegate.NewSpawner(nil, db.Stores{}, nil, nil, "")}

	cases := []struct {
		name                    string
		renew, selfFence, lease string
		wantRefused             bool
	}{
		{name: "defaults", wantRefused: false},
		{name: "renew at or past the fence", renew: "50", selfFence: "45", wantRefused: true},
		{name: "fence past the lease", selfFence: "80", lease: "75", wantRefused: true},
		{name: "fence equal to the lease", selfFence: "75", lease: "75", wantRefused: true},
		{name: "widened but ordered", renew: "30", selfFence: "90", lease: "150", wantRefused: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TF_CLAIM_RENEW_SEC", tc.renew)
			t.Setenv("TF_CLAIM_SELF_FENCE_SEC", tc.selfFence)
			t.Setenv("TF_CLAIM_TAKEOVER_SEC", tc.lease)

			err := a.buildReaper()
			if !tc.wantRefused {
				if err != nil {
					t.Fatalf("buildReaper = %v, want a clean boot", err)
				}
				return
			}
			if err == nil {
				t.Fatal("buildReaper accepted a configuration that lets a lease expire before its holder fences")
			}
			// The message has to name all three, because which one to move is
			// the operator's decision and two of the values are usually
			// defaults they never set.
			for _, knob := range []string{"TF_CLAIM_RENEW_SEC", "TF_CLAIM_SELF_FENCE_SEC", "TF_CLAIM_TAKEOVER_SEC"} {
				if !strings.Contains(err.Error(), knob) {
					t.Errorf("refusal %q does not name %s", err, knob)
				}
			}
		})
	}
}
