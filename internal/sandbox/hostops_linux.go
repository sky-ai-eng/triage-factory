//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
)

// defaultOps is the PrivilegedOps implementation every entry point in
// this package (wrap, Close, reapOrphansImpl) calls through. hostOps
// is the default — every privileged operation runs in-process with
// exactly the code that lived directly in those entry points before
// this seam existed. SetPrivilegedOps swaps this for an IPC client when
// TF_PRIVSEP=1, at zero call-site churn.
var defaultOps PrivilegedOps = hostOps{}

// SetPrivilegedOps replaces the package's PrivilegedOps implementation.
// The orchestrator's cap-broker wiring (cmd/capbroker) calls this once at
// boot — after starting and dialing the broker — to route every
// subsequent wrap() / Close() / reapOrphansImpl() through the IPC client
// instead of in-process hostOps. TF_PRIVSEP unset (the default) means
// this is never called and defaultOps stays hostOps{}: today's behavior,
// byte-identical.
//
// Not synchronized: it's a single boot-time call made before any Wrap or
// ReapOrphans runs, exactly like defaultOps's own package-level
// initialization above. Calling it after a sandbox is already live would
// race; callers must not.
//
// Panics on a nil ops: this is a boot-wiring call with exactly one
// production call site, which only ever passes a client it has already
// nil/error-checked. A nil defaultOps would otherwise fail confusingly —
// a nil-interface panic deep inside the first real Wrap — so a
// misconfigured caller finds out immediately, at the call site, instead.
func SetPrivilegedOps(ops PrivilegedOps) {
	if ops == nil {
		panic("sandbox: SetPrivilegedOps called with a nil PrivilegedOps")
	}
	defaultOps = ops
}

// NewHostOps returns the in-process PrivilegedOps implementation — the
// same one defaultOps uses by default. The cap-broker subcommand
// (cmd/capbroker) constructs one of these to serve the real
// netns/iptables/cgroup/rootfs operations over its RPC socket — the same
// in-process implementation this package has always used, reached
// through the exported constructor rather than the unexported hostOps
// type.
func NewHostOps() PrivilegedOps {
	return hostOps{}
}

// hostOps implements PrivilegedOps by calling the existing in-process
// helpers directly. It holds no state of its own — every method is a
// function of its arguments plus host state (files, netns, iptables),
// matching a stateless RPC handler shape ahead of the broker split.
type hostOps struct{}

// SetupNetwork composes the three network-setup helpers
// (netns_linux.go, iptables_linux.go) that together form the
// "Network" privileged-op bucket (spec §5): netns/veth creation,
// MASQUERADE, and the Part B egress allowlist.
func (hostOps) SetupNetwork(ctx context.Context, runID string, subnetIdx uint8) (NetworkState, error) {
	netSt, err := setupNetwork(ctx, runID, subnetIdx)
	state := networkStateFrom(netSt, subnetIdx)
	if err != nil {
		return state, err
	}

	rule, err := applyMasquerade(ctx, netSt.subnet, netSt.upstreamIF)
	if err != nil {
		return state, err
	}
	state.MasqueradeRule = rule

	egressRules, err := applyEgressPolicy(ctx, netSt.netnsName, netSt.vethHost, state.HostIP)
	if err != nil {
		return state, fmt.Errorf("egress policy: %w", err)
	}
	state.EgressRules = egressRules

	return state, nil
}

// networkStateFrom converts netns_linux.go's internal *netState (nil
// on total failure, partially populated on a mid-setup failure) into
// the serializable NetworkState the interface returns.
func networkStateFrom(netSt *netState, subnetIdx uint8) NetworkState {
	if netSt == nil {
		return NetworkState{}
	}
	return NetworkState{
		Subnet:      netSt.subnet,
		HostIP:      hostIP(subnetIdx),
		NetnsPath:   netSt.netnsPath,
		NetnsName:   netSt.netnsName,
		VethHost:    netSt.vethHost,
		VethSandbox: netSt.vethSandbox,
	}
}

func (hostOps) TeardownNetwork(ctx context.Context, state NetworkState) error {
	if err := teardownIptables(ctx, state.MasqueradeRule); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown iptables: %v\n", err)
	}
	// Host-side egress-allowlist rules (Part B). The in-netns OUTPUT
	// rules vanish with the netns delete below and need no explicit
	// removal.
	for _, r := range state.EgressRules {
		if err := teardownIptables(ctx, r); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: teardown egress rule: %v\n", err)
		}
	}
	netSt := &netState{
		netnsName:   state.NetnsName,
		netnsPath:   state.NetnsPath,
		vethHost:    state.VethHost,
		vethSandbox: state.VethSandbox,
	}
	if err := teardownNetwork(ctx, netSt); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown network: %v\n", err)
	}
	return nil
}

func (hostOps) EnsureRootfs(ctx context.Context, _ RootfsSelector) (string, error) {
	return ensureRootfs(ctx)
}

func (hostOps) SetupRunCgroup(name string, limitMB int) (string, *os.File, error) {
	return setupAndCreateRunCgroup(name, limitMB)
}

func (hostOps) RemoveRunCgroup(dir string) error {
	return removeRunCgroup(dir)
}

// ReapOrphans walks /var/run/netns for tf-<id>-<idx> entries and tears
// each down, then sweeps $TMPDIR for tf-bundle-* directories. Called
// once at TF startup (via reapOrphansImpl) so a hard-crashed previous
// TF process doesn't leak resources indefinitely.
//
// Per-entry failures are logged but not returned — best-effort cleanup
// is the contract.
//
// Subnet indices are MarkInUse'd before teardown and Released after,
// so a concurrent first-Wrap can't allocate the same idx while a sweep
// is mid-cleanup.
func (hostOps) ReapOrphans(ctx context.Context) error {
	const netnsDir = "/var/run/netns"

	entries, err := os.ReadDir(netnsDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No /var/run/netns/ at all — possible in a fresh
			// container before any `ip netns add` has ever run.
			// Nothing to reap.
			return reapBundleOrphans(ctx)
		}
		return err
	}

	for _, e := range entries {
		name := e.Name()
		idx, ok := subnetIdxFromNetnsName(name)
		if !ok {
			// Strict regex match prevents collateral damage to
			// unrelated tf-* names a different process might own.
			continue
		}

		sandboxLog.Info("reaping orphan netns", "netns", name, "subnet_idx", idx)

		defaultAllocator().MarkInUse(idx)

		netSt := &netState{
			netnsName: name,
			netnsPath: netnsDir + "/" + name,
			// vethHost / vethSandbox not knowable from the netns
			// name alone — but the kernel auto-removes the veth
			// pair when the netns is deleted, so teardownNetwork's
			// `ip link delete` fail-softs on the unknown name and
			// the subsequent `ip netns delete` does the actual work.
		}
		_ = teardownNetwork(ctx, netSt)

		// Match-by-subnet / match-by-gateway cleanup of the
		// MASQUERADE + Part B egress rules this orphan would have
		// installed. 10.42.0.0/16 is exclusively the sandbox's
		// allocation pool, so anything matching is unambiguously ours.
		reapIptablesForSubnet(ctx, subnetCIDR(idx))
		reapEgressForSubnet(ctx, idx)

		defaultAllocator().Release(idx)
	}

	// Leftover per-run memory-ceiling cgroups from a crashed process.
	// Empty groups rmdir; one still holding a live orphan runsc stays
	// for the next sweep.
	reapOrphanRunCgroups()

	return reapBundleOrphans(ctx)
}
