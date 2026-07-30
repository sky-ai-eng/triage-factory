package agentproc

import "testing"

// TestRegisterLiveJail_Lifecycle pins what the resource sampler reads: a
// registered jail is visible with its claim attached, the release drops it, and
// the release is idempotent — a registry entry outliving its jail would make
// the sampler read a removed cgroup on every tick from then on.
func TestRegisterLiveJail_Lifecycle(t *testing.T) {
	t.Cleanup(resetLiveJails)
	resetLiveJails()

	if got := LiveJails(); len(got) != 0 {
		t.Fatalf("registry should start empty, got %+v", got)
	}

	release := registerLiveJail("claim-a", "tf-run-a-1")
	got := LiveJails()
	if len(got) != 1 || got[0].ClaimID != "claim-a" || got[0].ContainerID != "tf-run-a-1" {
		t.Fatalf("LiveJails = %+v, want one claim-a/tf-run-a-1 entry", got)
	}

	release()
	if got := LiveJails(); len(got) != 0 {
		t.Fatalf("after release LiveJails = %+v, want empty", got)
	}
	release()
	if got := LiveJails(); len(got) != 0 {
		t.Fatalf("second release must be a no-op, got %+v", got)
	}
}

// TestRegisterLiveJail_ConcurrentJailsAreDistinct covers the reason the
// registry is keyed by container id: several jails run at once, each attributed
// to its own engagement, and the snapshot is ordered so repeated reads are
// stable.
func TestRegisterLiveJail_ConcurrentJailsAreDistinct(t *testing.T) {
	t.Cleanup(resetLiveJails)
	resetLiveJails()

	releaseB := registerLiveJail("claim-b", "tf-run-b-2")
	registerLiveJail("claim-a", "tf-run-a-1")
	registerLiveJail("claim-c", "tf-run-c-3")

	got := LiveJails()
	want := []LiveJail{
		{ClaimID: "claim-a", ContainerID: "tf-run-a-1"},
		{ClaimID: "claim-b", ContainerID: "tf-run-b-2"},
		{ClaimID: "claim-c", ContainerID: "tf-run-c-3"},
	}
	if len(got) != len(want) {
		t.Fatalf("LiveJails = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LiveJails[%d] = %+v, want %+v (snapshot must be ordered by container id)", i, got[i], want[i])
		}
	}

	// One jail ending leaves its siblings alone.
	releaseB()
	got = LiveJails()
	if len(got) != 2 || got[0].ContainerID != "tf-run-a-1" || got[1].ContainerID != "tf-run-c-3" {
		t.Fatalf("after releasing one jail LiveJails = %+v, want the other two", got)
	}
}

// TestRegisterLiveJail_UnattributableLaunchIsNotRegistered covers the toolless
// system jobs (no claim) and a non-Linux Wrap (no container): the sampler has
// nowhere to attribute their usage, so they must not appear at all rather than
// appear with a blank key.
func TestRegisterLiveJail_UnattributableLaunchIsNotRegistered(t *testing.T) {
	t.Cleanup(resetLiveJails)
	resetLiveJails()

	cases := map[string][2]string{
		"no claim (system job)":    {"", "tf-scorer-batch-1"},
		"no container (non-Linux)": {"claim-a", ""},
		"neither":                  {"", ""},
	}
	for name, ids := range cases {
		release := registerLiveJail(ids[0], ids[1])
		if got := LiveJails(); len(got) != 0 {
			t.Errorf("%s: LiveJails = %+v, want empty", name, got)
		}
		release() // must be safe even though nothing was recorded
	}
}

// resetLiveJails clears the process-wide registry between tests.
func resetLiveJails() {
	liveJails.mu.Lock()
	liveJails.byContainer = make(map[string]string)
	liveJails.mu.Unlock()
}
