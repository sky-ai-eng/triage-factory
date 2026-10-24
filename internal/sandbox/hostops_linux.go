//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// defaultOps is the PrivilegedOps implementation the package's
// privileged-op entry points (wrap, Close, reapOrphansImpl, the run-tree
// helpers) call through. hostOps is the placeholder default; on any host
// that actually sandboxes (multi + Linux) the orchestrator installs the
// cap-broker IPC client via SetPrivilegedOps at boot, before any run, so
// every privileged op crosses to the broker.
var defaultOps PrivilegedOps = hostOps{}

// runLauncher is the run-launch seam — nil until the broker is installed.
// The launch is always brokered (there is no in-process launcher), so a
// wrap() reached before SetPrivilegedOps ran is a boot-order bug that
// fails loudly rather than silently launching in a less-isolated way.
var runLauncher RunLauncher

// sidecarLauncher is the sidecar-launch seam, installed alongside
// runLauncher — same boot-order contract: nil until SetPrivilegedOps runs,
// and a LaunchSidecar reached before that is a boot-order bug.
var sidecarLauncher SidecarLauncher

// runCapturer is the capture-jail seam, installed alongside runLauncher with
// the same boot-order contract. A capture reached before SetPrivilegedOps ran
// fails rather than running anywhere else.
var runCapturer RunCapturer

// SetPrivilegedOps installs the cap-broker IPC client as BOTH the
// package's privileged-op implementation and its run launcher. The
// orchestrator's cap-broker wiring (cmd/capbroker) calls this once at boot
// — after starting and dialing the broker — so every subsequent wrap() /
// Close() / reapOrphansImpl() / run-tree op and every run launch routes
// through the broker. The launch is always brokered, so ops must satisfy
// RunLauncher too (the IPC client does).
//
// Not synchronized: it's a single boot-time call made before any Wrap or
// ReapOrphans runs, exactly like defaultOps's own package-level
// initialization above. Calling it after a sandbox is already live would
// race; callers must not.
//
// Panics on a nil ops: this is a boot-wiring call with exactly one
// production call site, which only ever passes a client it has already
// nil/error-checked. A nil install would otherwise fail confusingly — a
// nil-interface panic deep inside the first real Wrap — so a misconfigured
// caller finds out immediately, at the call site, instead.
func SetPrivilegedOps(ops SandboxOps) {
	if ops == nil {
		panic("sandbox: SetPrivilegedOps called with a nil SandboxOps")
	}
	defaultOps = ops
	runLauncher = ops
	sidecarLauncher = ops
	runCapturer = ops
}

// NewHostOps returns the in-process PrivilegedOps implementation. The
// cap-broker subcommand (cmd/capbroker) constructs one of these to serve
// the real netns/iptables/rootfs/run-tree operations over its RPC socket,
// reached through the exported constructor rather than the unexported
// hostOps type.
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
//
// A setup that fails removes what it built before it returns. The caller
// gets only the error, through the broker or in process, and a network left
// half-built would outlive it: its namespace stays live in the ledger,
// refusing every retry of the run, and its gateway's route shadows the next
// network given its index.
func (hostOps) SetupNetwork(ctx context.Context, conversationID string, subnetIdx uint8) (NetworkState, error) {
	netSt, err := setupNetwork(ctx, conversationID, subnetIdx)
	state := networkStateFrom(netSt, subnetIdx)
	if err == nil {
		err = secureNetwork(ctx, netSt, state, subnetIdx)
	}
	if err != nil {
		abandonNetwork(ctx, state)
		return NetworkState{}, err
	}
	return state, nil
}

// secureNetwork is the part of SetupNetwork after the network exists: its
// route out, its egress policy, and its jails' /tmp.
func secureNetwork(ctx context.Context, netSt *netState, state NetworkState, subnetIdx uint8) error {
	if err := applyMasquerade(ctx, netSt.subnet, netSt.upstreamIF); err != nil {
		return err
	}
	if err := applyEgressPolicy(ctx, netSt.netnsName, netSt.vethHost, state.HostIP); err != nil {
		return fmt.Errorf("egress policy: %w", err)
	}
	// The network's jails mount one /tmp, keyed by its index. It is emptied
	// here rather than at each launch, so a jail relaunched into this network
	// keeps what the one before it left, and a network on a reused index
	// starts with nothing of an earlier one's.
	if _, err := prepareJailTmp(ctx, subnetIdx); err != nil {
		return err
	}
	return nil
}

// abandonNetworkTimeout bounds the teardown of a network whose setup failed.
const abandonNetworkTimeout = 30 * time.Second

// abandonNetwork tears down what a failed setup built. It does not inherit
// ctx's cancellation, because a setup that failed when ctx ended still has a
// network to remove.
func abandonNetwork(ctx context.Context, state NetworkState) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonNetworkTimeout)
	defer cancel()
	if err := (hostOps{}).TeardownNetwork(ctx, state); err != nil {
		sandboxLog.Warn("network of a failed setup not torn down", "netns", state.NetnsName, "error", err)
	}
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

// TeardownNetwork reverses SetupNetwork. state comes from the caller, so no
// name or rule in it reaches a command: the network is identified by its
// namespace name, which has to be one SetupNetwork builds, and state has to be
// exactly the state SetupNetwork reports for that network. Anything else is
// refused before anything runs, so a forged state cannot aim the deletes at a
// host interface, a namespace the broker did not build, or another network's
// firewall rules. The rules are then found by subnet, veth and gateway, as the
// boot reap finds a leaked network's.
func (hostOps) TeardownNetwork(ctx context.Context, state NetworkState) error {
	netSt, idx, err := teardownTarget(state)
	if err != nil || netSt == nil {
		return err
	}
	reapIptablesForSubnet(ctx, netSt.subnet)
	if err := teardownNetwork(ctx, netSt); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown network: %v\n", err)
	}
	// The in-netns OUTPUT rules went with the netns; the host-side ones are
	// removed here.
	removeHostEgressRules(ctx, netSt.vethHost, hostIP(idx))
	// The jails of this network are stopped by now, and their /tmp is keyed
	// by the index about to be released, so it goes with the network.
	if err := removeJailTmp(ctx, idx); err != nil {
		sandboxLog.Warn("jail /tmp not removed with its network", "subnet_idx", idx, "error", err)
	}
	return nil
}

// teardownTarget is the network TeardownNetwork removes for state, and its
// subnet index. A zero state, which is what a setup that failed before
// building anything reports, names nothing to remove.
func teardownTarget(state NetworkState) (*netState, uint8, error) {
	if state == (NetworkState{}) {
		return nil, 0, nil
	}
	netSt, idx, ok := runNetFromNetnsName(state.NetnsName)
	if !ok {
		return nil, 0, fmt.Errorf("sandbox: teardown network: %q is not a run network's namespace", state.NetnsName)
	}
	if state != networkStateFrom(netSt, idx) {
		return nil, 0, fmt.Errorf("sandbox: teardown network: the state does not match run network %s", state.NetnsName)
	}
	return netSt, idx, nil
}

// removeHostEgressRules removes the host-side egress rules of veth, whose
// gateway is gw, once that veth is gone. A veth that survived its delete keeps
// its rules, closed as they were. The rules are found by listing the chains,
// not from a record, so an admission whose reply never reached the
// orchestrator is removed too, and an ACCEPT left behind cannot open its port
// to the next network given this veth's name.
func removeHostEgressRules(ctx context.Context, veth, gw string) {
	if _, err := net.InterfaceByName(veth); err == nil {
		sandboxLog.Warn("run veth survived its teardown; leaving its egress rules in place", "veth", veth)
		return
	}
	reapEgressRules(ctx, gw, veth)
	removeIPv6Drops(ctx, veth)
}

func (hostOps) AdmitJailPorts(ctx context.Context, conversationID string, subnetIdx uint8, ports []int) error {
	if err := ValidateJailPorts(ports); err != nil {
		return err
	}
	// The network must be one this process built for this conversation and
	// has not torn down. The veth name carries only a short fragment of the
	// conversation id, so it alone could name another conversation's
	// network at the same index; the ledger holds the whole id.
	if owner, live := netnsLiveRun(NetnsNameForRun(conversationID, subnetIdx)); !live || owner != conversationID {
		return fmt.Errorf("sandbox: admit jail ports: no live run network for this conversation at subnet index %d", subnetIdx)
	}
	// And the veth must exist: a rule naming an interface that does not is
	// accepted by iptables and matches nothing.
	vethHost := vethHostName(conversationID, subnetIdx)
	if _, err := net.InterfaceByName(vethHost); err != nil {
		return fmt.Errorf("sandbox: admit jail ports: no run network %s: %w", vethHost, err)
	}
	if err := admitJailPorts(ctx, vethHost, hostIP(subnetIdx), ports); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return nil
}

func (hostOps) EnsureRootfs(ctx context.Context, selector RootfsSelector) (string, error) {
	return resolveCatalogRootfs(ctx, selector)
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

	// The IPv6 drops name only a veth, so they are swept by the veth's name
	// shape rather than per namespace above; the veths of the namespaces
	// just reaped are gone, and a rule whose veth still exists is kept.
	removeIPv6Drops(ctx, "")

	// Leftover per-run memory-ceiling cgroups from a crashed process.
	// Empty groups rmdir; one still holding a live orphan runsc stays
	// for the next sweep.
	reapOrphanRunCgroups()

	// Leftover run-sidecar processes from a crashed broker — reparented to
	// init, unknown to this fresh process's runs registry, safe to
	// unconditionally SIGKILL because this whole method runs once, at boot,
	// before any run exists.
	reapOrphanSidecars()

	// Leftover per-run socket directories from a crashed process. They carry
	// sidecar uids and jail gids this boot will hand out again from index
	// zero, so they go before any of them is.
	reapOrphanRunSockets()

	// Leftover jail /tmp directories, for the same reason: each is keyed by a
	// subnet index this boot hands out again.
	reapOrphanJailTmp(ctx)

	// Leftover runsc container state from a crashed broker. Unlike the
	// sweeps above, this one has no natural collision to rely on: the ids
	// are keyed to subnet indices a fresh boot allocates from zero in a
	// different order, so state nobody sweeps is state nothing ever reclaims.
	reapOrphanRuntimeState(ctx)

	return reapBundleOrphans(ctx)
}
