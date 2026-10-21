package delegate

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
)

// namedJail is a cell.Jail told apart by name; the registry only stores it.
type namedJail struct {
	cell.Jail
	name string
}

func claims(jails []liveJail) []string {
	out := make([]string, len(jails))
	for i, j := range jails {
		out[i] = j.claimID
	}
	return out
}

// TestLiveJails_Lifecycle pins what the resource sampler reads: a registered
// jail is visible with its claim attached, the release drops it, and the
// release is idempotent — an entry outliving its jail would have the sampler
// read a jail that is gone on every tick from then on.
func TestLiveJails_Lifecycle(t *testing.T) {
	var r liveJails
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("registry should start empty, got %+v", got)
	}

	jail := &namedJail{name: "a"}
	release := r.add("claim-a", jail)
	got := r.snapshot()
	if len(got) != 1 || got[0].claimID != "claim-a" || got[0].jail != jail {
		t.Fatalf("snapshot = %+v, want the one claim-a jail", got)
	}

	release()
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("after release snapshot = %+v, want empty", got)
	}
	release()
	if got := r.snapshot(); len(got) != 0 {
		t.Fatalf("second release must be a no-op, got %+v", got)
	}
}

// TestLiveJails_ConcurrentJailsAreDistinct covers several jails running at
// once, each attributed to its own engagement, with the snapshot ordered so
// repeated reads are stable, and one jail ending leaving its siblings alone.
func TestLiveJails_ConcurrentJailsAreDistinct(t *testing.T) {
	var r liveJails
	releaseB := r.add("claim-b", &namedJail{name: "b"})
	r.add("claim-a", &namedJail{name: "a"})
	r.add("claim-c", &namedJail{name: "c"})

	if got, want := claims(r.snapshot()), []string{"claim-a", "claim-b", "claim-c"}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("snapshot claims = %v, want %v in that order", got, want)
	}

	releaseB()
	if got := claims(r.snapshot()); len(got) != 2 || got[0] != "claim-a" || got[1] != "claim-c" {
		t.Fatalf("after releasing one jail snapshot claims = %v, want the other two", got)
	}
}

// TestLiveJails_UnattributableLaunchIsNotRegistered covers a launch with no
// claim (the test-harness shape) and no jail: the sampler has nowhere to
// attribute their usage, so they must not appear at all rather than appear
// with a blank key.
func TestLiveJails_UnattributableLaunchIsNotRegistered(t *testing.T) {
	var r liveJails
	for name, tc := range map[string]struct {
		claimID string
		jail    cell.Jail
	}{
		"no claim": {"", &namedJail{name: "harness"}},
		"no jail":  {"claim-a", nil},
		"neither":  {"", nil},
	} {
		release := r.add(tc.claimID, tc.jail)
		if got := r.snapshot(); len(got) != 0 {
			t.Errorf("%s: snapshot = %+v, want empty", name, got)
		}
		release() // must be safe even though nothing was recorded
	}
}
