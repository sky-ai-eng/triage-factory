package app

import (
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
)

// TestBuildReaper_SelfFenceOrdering pins the boot refusal. A partitioned
// executor must have provably stopped claiming and killed its sandboxes
// before the reaper may requeue rows it might still be writing; a
// configuration whose fence lands at or after the staleness threshold cannot
// guarantee that, and there is no safe way to run it.
func TestBuildReaper_SelfFenceOrdering(t *testing.T) {
	cases := []struct {
		name             string
		selfFence, stale string
		wantRefused      bool
	}{
		{name: "defaults"},
		{name: "fence past the threshold", selfFence: "60", stale: "45", wantRefused: true},
		{name: "fence equal to the threshold", selfFence: "45", stale: "45", wantRefused: true},
		{name: "widened but ordered", selfFence: "60", stale: "120"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{spawner: delegate.NewSpawner(nil, db.Stores{}, nil, nil, "")}
			t.Setenv("TF_SELF_FENCE_SEC", tc.selfFence)
			t.Setenv("TF_REAPER_STALE_SEC", tc.stale)

			err := a.buildReaper()
			if !tc.wantRefused {
				if err != nil {
					t.Fatalf("buildReaper = %v, want a clean boot", err)
				}
				return
			}
			if err == nil {
				t.Fatal("buildReaper accepted a configuration that lets the reaper requeue rows an executor may still be writing")
			}
			// Which of the two to move is the operator's decision, so the
			// refusal has to name both values rather than just the verdict.
			if !strings.Contains(err.Error(), "self-fence deadline") || !strings.Contains(err.Error(), "staleness threshold") {
				t.Errorf("refusal %q does not name both sides of the ordering", err)
			}
		})
	}
}
