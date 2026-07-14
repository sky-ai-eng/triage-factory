//go:build linux && integration

// Process-exit eviction proof (PRCI-P4 work item 1). The doc claims that when a
// run ends — normal, cancel, crash, or park — its credential sidecar exits and
// the credential-bearing address space is freed. This proves that claim directly
// from the kernel: the run's whole credential-bearing process family is exactly
// the set of pids at SidecarUID(subnetIdx) (a per-run key, one uid per live run
// from the subnet allocator), so the property is "that uid band is occupied
// while the run is live and empty once it is torn down" — including any node
// children, and needing no Pid() accessor on the orchestrator-held sidecar
// handle.
//
// Address-space teardown, not memory zeroing, is the eviction mechanism: process
// exit frees the whole mapping (the sidecar's own cmd/runsidecar shutdown drains
// the relocated agenthost daemon first, but process teardown supersedes any
// explicit zeroing). Go makes reliable in-place zeroing impractical, which is
// why the claimable guarantee is process death, not scrubbing — do not
// reintroduce long-lived sidecar reuse expecting a scrub to stand in for it.
//
// Coverage split (accepted): at this layer the four named run-exit paths all
// collapse to one teardown trigger — LaunchedSidecar.Close() / context death —
// so this proves real-process eviction on that generic teardown. That each named
// path (normal/cancel/crash/park) routes to that teardown is covered at the
// internal/delegate layer with doubles; this does not re-drive a full real run
// through each path.

package sandbox

import (
	"context"
	"testing"
	"time"
)

// TestIntegration_SidecarEviction_UIDBandVacates stands up two live runs, then
// evicts one and proves its per-run credential uid band empties while the other
// run's is untouched — eviction is real and per-run, not global.
func TestIntegration_SidecarEviction_UIDBandVacates(t *testing.T) {
	requireRunsc(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a := startLiveRun(t, ctx, "itest-sc-a", nil)
	b := startLiveRun(t, ctx, "itest-sc-b", nil)

	// Distinct per-run uids follow directly from distinct subnet indices.
	if a.sidecarUID == b.sidecarUID {
		t.Fatalf("two live runs share sidecar uid %d — the subnet allocator handed out a duplicate index", a.sidecarUID)
	}

	// While live, each run's uid band holds at least one real, credential-bearing
	// process — there is something to evict.
	for _, lr := range []*liveRun{a, b} {
		if pids := pidsAtUID(t, lr.sidecarUID); len(pids) == 0 {
			t.Fatalf("run %s: no live process at sidecar uid %d while the run is up — the sidecar never came up", lr.runID, lr.sidecarUID)
		}
	}

	// Evict run A via the generic teardown every run-exit path collapses to at
	// this layer. Its uid band must empty (address space freed, process reaped)…
	if err := a.sidecar.Close(); err != nil {
		t.Fatalf("evict sidecar A: %v", err)
	}
	waitForUIDBandEmpty(t, a.sidecarUID, 5*time.Second)

	// …while run B's sidecar is untouched: eviction frees one run, not the box.
	if pids := pidsAtUID(t, b.sidecarUID); len(pids) == 0 {
		t.Fatalf("evicting run A also emptied run B's sidecar uid %d — eviction is not per-run", b.sidecarUID)
	}
}
