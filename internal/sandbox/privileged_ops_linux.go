//go:build linux

package sandbox

import (
	"context"
	"os"
)

// PrivilegedOps is the seam between sandbox orchestration and every
// operation in this package that needs root-equivalent Linux
// capabilities (CAP_NET_ADMIN, CAP_SYS_ADMIN, CAP_SYS_CHROOT): network
// setup/teardown, cgroup create/remove, rootfs bake, and boot-time
// orphan reap.
//
// Every method mirrors the RPC vocabulary in
// docs/specs/privsep/README.md §4 so a future out-of-process cap-broker
// (PS-P1) can implement this same interface over a socket client at
// zero call-site churn. Parameters and returns are therefore restricted
// to serializable values (strings, ints, structs) that can cross an
// eventual JSON RPC unchanged — no live *exec.Cmd, no *os.File — with
// one documented exception: SetupRunCgroup's returned fd, which is only
// meaningful to a caller in the same process (see its doc).
//
// wrap(), Close(), and reapOrphansImpl() — the three entry points the
// PS-P0 audit named — call only through this interface for every
// privileged operation. hostOps (hostops_linux.go) is the sole
// implementation today and runs each method in-process with exactly
// the code that lived directly in those entry points before this seam
// existed. The runsc launch itself (newRunscCommand + runsc run +
// stdio) is explicitly out of scope here — see spec §3.1 — and stays
// caller-side in wrap() until PS-P2.
type PrivilegedOps interface {
	// SetupNetwork creates the per-run netns + veth pair, applies the
	// MASQUERADE + egress-allowlist iptables rules, and ensures
	// ip_forward is enabled — the full "Network" bucket from the
	// privileged-op audit (spec §5). Returns the state TeardownNetwork
	// needs to reverse it; on error the returned state carries
	// whatever prefix of setup succeeded, so a caller that stores it
	// unconditionally (even on error) can still clean up partial state.
	SetupNetwork(ctx context.Context, runID string, subnetIdx uint8) (NetworkState, error)

	// TeardownNetwork reverses SetupNetwork: removes the iptables
	// rules, deletes the veth pair + netns. Idempotent and best-effort
	// — safe to call against partial or zero-value state.
	TeardownNetwork(ctx context.Context, state NetworkState) error

	// EnsureRootfs idempotently bakes (or returns the cached path to)
	// the curated rootfs matching selector. v1 has exactly one curated
	// variant; selector exists now so a broker-owned catalog (spec §8)
	// can grow without changing this signature later.
	EnsureRootfs(ctx context.Context, selector RootfsSelector) (rootfsPath string, err error)

	// SetupRunCgroup creates the per-run memory-ceiling cgroup and
	// returns its directory plus an open dir-fd for exec.Cmd's
	// CgroupFD. The fd is the one documented exception to "no live
	// *os.File crosses the interface": it is only usable by a caller
	// that Starts the runsc process in the same address space as this
	// call — exactly what wrap() does today. PS-P2 folds cgroup
	// creation into the broker's own LaunchRun, at which point the fd
	// never needs to leave the implementation at all.
	SetupRunCgroup(name string, limitMB int) (dir string, cgroupFD *os.File, err error)

	// RemoveRunCgroup tears down the group SetupRunCgroup created.
	RemoveRunCgroup(dir string) error

	// ReapOrphans sweeps leftover netns/veth/iptables/cgroup state left
	// by a previous, hard-crashed process. Called once at TF startup.
	ReapOrphans(ctx context.Context) error
}

// NetworkState is the serializable per-run network state SetupNetwork
// returns and TeardownNetwork consumes. Every field is a plain string
// or a small value struct so the whole thing round-trips as JSON over
// a future broker RPC.
type NetworkState struct {
	// Subnet, HostIP, NetnsPath are SetupNetwork's documented return
	// per spec §4.
	Subnet    string
	HostIP    string
	NetnsPath string

	// The rest is teardown-only bookkeeping. A future broker keeps this
	// broker-internal once Teardown becomes a bare run-id RPC (PS-P3);
	// P0 has no broker process to hold it, so it travels alongside the
	// documented fields so this same process can reverse its own setup
	// without re-deriving names.
	NetnsName      string
	VethHost       string
	VethSandbox    string
	MasqueradeRule iptablesRule
	EgressRules    []iptablesRule
}

// RootfsSelector names which curated rootfs variant EnsureRootfs
// should build/return. v1 has exactly one variant (rootfs_linux.go's
// alpine + apkPackages bake); Name is unused today and exists only so
// the signature doesn't need to change when the broker's catalog
// (spec §8) grows past one entry.
type RootfsSelector struct {
	Name string
}
