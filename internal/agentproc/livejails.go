package agentproc

import (
	"sort"
	"sync"
)

// The live-jail registry: which sandboxed launches are running in this process
// right now, and which engagement pays for each.
//
// It exists so an observer on a timer — the executor's resource sampler — can
// ask "what jails are live?" without holding a reference to any run. The
// registry is process-wide rather than threaded through each caller's options
// because LaunchToolHost is the single choke point every sandboxed launch
// passes through (the native runtime's every engagement), so registering
// there covers every surface at once and can't drift as surfaces
// are added. A per-surface callback would have to be wired identically at each
// call site to say the same thing.
//
// Keyed by container id, not claim id: the container id is unique per live
// Wrap by construction (the subnet allocator hands out a fresh index for
// each), so two launches can never clobber each other's entry. A launch with
// no claim to attribute — the system jobs — is not registered at all: an
// unattributable jail's usage has nowhere to go.

// LiveJail names one sandboxed launch live in this process: the engagement
// that pays for it, and the container id its cgroup is named after.
type LiveJail struct {
	ClaimID     string
	ContainerID string
}

var liveJails = struct {
	mu sync.Mutex
	// byContainer maps container id → claim id.
	byContainer map[string]string
}{byContainer: make(map[string]string)}

// registerLiveJail records a launch as live and returns the release that drops
// it again. Both are no-ops when either id is empty (an unattributable launch,
// or a non-Linux Wrap that produced no container), so a caller can register
// unconditionally and defer the release the same way.
//
// The release is idempotent — teardown paths in this package run through a
// single-shot cleanup, but a registry entry outliving its jail would make the
// sampler read a removed cgroup forever, so it does not rely on that.
func registerLiveJail(claimID, containerID string) (release func()) {
	if claimID == "" || containerID == "" {
		return func() {}
	}
	liveJails.mu.Lock()
	liveJails.byContainer[containerID] = claimID
	liveJails.mu.Unlock()
	return func() {
		liveJails.mu.Lock()
		delete(liveJails.byContainer, containerID)
		liveJails.mu.Unlock()
	}
}

// LiveJails returns a snapshot of every attributable sandboxed launch live in
// this process, ordered by container id so repeated reads are stable.
//
// Empty is the common answer and the cheap one: a control pod never launches a
// jail, and an idle executor has none live, so a sampler that iterates this
// does no work at all when there is nothing running.
func LiveJails() []LiveJail {
	liveJails.mu.Lock()
	out := make([]LiveJail, 0, len(liveJails.byContainer))
	for containerID, claimID := range liveJails.byContainer {
		out = append(out, LiveJail{ClaimID: claimID, ContainerID: containerID})
	}
	liveJails.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ContainerID < out[j].ContainerID })
	return out
}
