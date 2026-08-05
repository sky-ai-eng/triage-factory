//go:build linux

package sandbox

import "sync"

// The privileged side's own record of which network namespaces it created and
// has not yet torn down.
//
// It exists because reclaiming a stale namespace means DELETING one, and that
// deletion is permitted by privilege rather than by ownership. Every other
// remove-first in this cell is bounded by something the kernel checks: the
// orchestrator's file sweeps unlink entries in a directory it owns, and the
// teardown sweep additionally refuses an entry owned by another cell's uid, so
// a caller with the wrong idea about what is stale simply fails. A namespace
// has no owner to check — the broker holds CAP_NET_ADMIN and the kernel will
// delete whatever name it is handed — so the bound has to be state the
// privileged side keeps for itself.
//
// The alternative is to take the caller's word for it. `SetupNetwork` is an RPC
// whose run id and subnet index arrive from the orchestrator unvalidated, and
// the argument that the pair cannot name a live cell is a property of the
// ORCHESTRATOR's in-process subnet allocator — the wrong side of the boundary
// for the one operation here that destroys something. This is the
// broker-internal network state validateNetnsPath's residual-risk note names as
// what per-run netns ownership ultimately needs; it is used here for the narrow
// question the delete asks, not yet as launch-time ownership.
//
// Process-local, like the subnet allocator beside it: meaningful only in
// whichever process runs the real privileged ops, which in a sandboxing
// deployment is the cap-broker and nothing else. A broker restart empties it,
// which is correct — every namespace it knew about is stale by then, and the
// boot reap sweeps them.
var liveNetns = struct {
	mu     sync.Mutex
	byName map[string]string // netns name → the run id it was created for
}{byName: make(map[string]string)}

// markNetnsLive records that this process created name for runID. Called once
// the namespace actually exists, so the ledger never claims one that failed to
// come up.
func markNetnsLive(name, runID string) {
	if name == "" {
		return
	}
	liveNetns.mu.Lock()
	defer liveNetns.mu.Unlock()
	liveNetns.byName[name] = runID
}

// forgetNetns drops name from the ledger. Called from teardown BEFORE the
// delete is attempted, and unconditionally: a teardown whose `ip netns delete`
// fails is exactly how a namespace leaks, and that leak is what a later
// bring-up must be free to reclaim. Keeping a failed teardown "live" would
// wedge that (runID, index) pair until the process restarted.
func forgetNetns(name string) {
	if name == "" {
		return
	}
	liveNetns.mu.Lock()
	defer liveNetns.mu.Unlock()
	delete(liveNetns.byName, name)
}

// netnsLiveRun reports the run id name was created for, if this process still
// believes the namespace is live.
func netnsLiveRun(name string) (string, bool) {
	liveNetns.mu.Lock()
	defer liveNetns.mu.Unlock()
	runID, live := liveNetns.byName[name]
	return runID, live
}
