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
// in-process implementation; the cap-broker's socket client is the
// out-of-process one. The runsc launch is LaunchRun: the in-process
// implementation execs runsc with ordinary pipes; the broker execs and
// supervises it with its stdio wired to a passed-through socket.
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

	// LaunchRun execs+supervises the gVisor runtime for one prepared
	// bundle and returns a LaunchedRun the caller drives. The in-process
	// implementation (hostOps) builds the runsc command with ordinary
	// pipes for stdio and creates the memory cgroup locally; the broker
	// implementation execs+supervises runsc in the broker process with its
	// stdio wired to a passed-through socket, so the run's bytes never
	// enter the (unprivileged) orchestrator's peer holder. The per-run
	// memory cgroup is created and torn down by whichever side execs the
	// runtime — the cgroup fd is needed only at that side's clone3 and
	// never crosses the interface.
	LaunchRun(ctx context.Context, p LaunchParams) (LaunchedRun, error)

	// SetupRunCgroup creates the per-run memory-ceiling cgroup and
	// returns its directory plus an open dir-fd for exec.Cmd's
	// CgroupFD. Retained from the P1 seam; the runsc launch path now
	// creates its cgroup inside LaunchRun (the fd stays on whichever side
	// execs the runtime), so this is exercised by the P1 conformance tests
	// rather than the live launch path.
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

// LaunchParams is the serializable input to LaunchRun: everything the
// runtime launcher needs to exec `runsc run` against an already-prepared
// bundle. Every field is a plain string/int so it round-trips as JSON
// over the broker RPC unchanged — no live *exec.Cmd, no *os.File, no
// io.Writer. Stdio does not appear here: the in-process launcher wires
// pipes internally, and the broker learns the per-run stdio socket path
// from its own client (which owns the listener), not from this struct.
type LaunchParams struct {
	// RunID identifies the run (telemetry + the broker's run registry key
	// for wait/kill).
	RunID string

	// BundleDir is the on-disk OCI bundle `runsc run --bundle` targets.
	// The bundle's config.json already references the pre-created netns, so
	// runsc joins it without any separate netns parameter.
	BundleDir string

	// ContainerID is the runsc container id (unique per Wrap) and the
	// per-run cgroup name.
	ContainerID string

	// MemoryLimitMB, when > 0, caps the run via a per-run memory cgroup the
	// launcher creates (fail-open, as before). Zero disables.
	MemoryLimitMB int
}
