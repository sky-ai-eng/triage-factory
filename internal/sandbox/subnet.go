package sandbox

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"sync"
)

// subnetBase is the /16 from which per-run /24s are carved. Choice
// of 10.42.0.0/16 follows the validation probe's 192.168.99.0/24
// convention but moves into RFC 1918 space that's less likely to
// collide with host networking (192.168.99.* is sometimes used by
// HomeKit / mDNS setups; 10.42.* is much rarer).
//
// Each /24 looks like 10.42.<N>.0/24:
//
//	10.42.<N>.1  = host-side veth IP (where the proxies will bind)
//	10.42.<N>.2  = sandbox-side veth IP
//	10.42.<N>.3+ = reserved for additional host-side bindings (multi-proxy)
const subnetBase = "10.42"

// MaxSandboxes is the structural ceiling on concurrent sandboxes per
// process: one /24 out of the 10.42.0.0/16 pool per sandbox, so at most
// this many can exist on a host at once. Exported so callers that need to
// clamp a concurrency knob to what the allocator can actually honor
// (internal/delegate's dispatcher cap) reference this instead of a second
// hardcoded 256 that could drift from the allocator's real limit.
const MaxSandboxes = 256

// allocator manages a process-wide free list of subnet indices in
// [0, MaxSandboxes). Each index N maps to the /24 10.42.<N>.0/24.
type allocator struct {
	mu   sync.Mutex
	free [MaxSandboxes]bool // true = free; initialized via newAllocator
}

func newAllocator() *allocator {
	a := &allocator{}
	for i := range a.free {
		a.free[i] = true
	}
	return a
}

// Allocate returns the next free subnet index, marking it taken.
// Returns ErrSubnetsExhausted when every index is in use.
func (a *allocator) Allocate() (uint8, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < MaxSandboxes; i++ {
		if a.free[i] {
			a.free[i] = false
			return uint8(i), nil
		}
	}
	return 0, ErrSubnetsExhausted
}

// MarkInUse forces an index into the "taken" state. Used by
// ReapOrphans before it tears down a discovered orphan netns: we
// claim the slot so a concurrent Allocate() doesn't reuse it
// mid-cleanup. After ReapOrphans finishes the cleanup, it Releases.
func (a *allocator) MarkInUse(idx uint8) {
	a.mu.Lock()
	a.free[idx] = false
	a.mu.Unlock()
}

// Release returns an index to the free pool. Idempotent — releasing
// an already-free index is a no-op.
func (a *allocator) Release(idx uint8) {
	a.mu.Lock()
	a.free[idx] = true
	a.mu.Unlock()
}

// IsFree reports whether the index is currently free. Used by tests
// to assert allocator state.
func (a *allocator) IsFree(idx uint8) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.free[idx]
}

// subnetCIDR returns the /24 string for index N — e.g. "10.42.7.0/24".
func subnetCIDR(idx uint8) string {
	return fmt.Sprintf("%s.%d.0/24", subnetBase, idx)
}

// hostIP returns the host-side veth IP for index N — e.g. "10.42.7.1".
func hostIP(idx uint8) string {
	return fmt.Sprintf("%s.%d.1", subnetBase, idx)
}

// sandboxIP returns the sandbox-side veth IP for index N — e.g. "10.42.7.2".
func sandboxIP(idx uint8) string {
	return fmt.Sprintf("%s.%d.2", subnetBase, idx)
}

// parseSubnetIdx extracts the third octet from a 10.42.N.x address.
// Used by ReapOrphans to reclaim subnet slots from netns names that
// were created in a previous TF process. Returns false if the IP
// doesn't match the expected base.
func parseSubnetIdx(ipStr string) (uint8, bool) {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return 0, false
	}
	if ip[0] != 10 || ip[1] != 42 {
		return 0, false
	}
	return ip[2], true
}

// subnetIdxFromNetnsName extracts the embedded subnet index from a
// per-run netns name (tf-<runID>-N where N is the 1-byte index in
// decimal). Returns false if the name doesn't match. Used by the
// reaper to figure out which allocator slot to release.
var netnsNameRE = regexp.MustCompile(`^tf-[0-9a-f]+-(\d{1,3})$`)

func subnetIdxFromNetnsName(name string) (uint8, bool) {
	m := netnsNameRE.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 || n > 255 {
		return 0, false
	}
	return uint8(n), true
}
