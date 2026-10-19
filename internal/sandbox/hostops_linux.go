//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
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
func (hostOps) SetupNetwork(ctx context.Context, conversationID string, subnetIdx uint8) (NetworkState, error) {
	netSt, err := setupNetwork(ctx, conversationID, subnetIdx)
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
	netSt := &netState{
		netnsName:   state.NetnsName,
		netnsPath:   state.NetnsPath,
		vethHost:    state.VethHost,
		vethSandbox: state.VethSandbox,
	}
	if err := teardownNetwork(ctx, netSt); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: teardown network: %v\n", err)
	}
	// The in-netns OUTPUT rules went with the netns; the host-side ones are
	// removed here.
	removeHostEgressRules(ctx, state)
	return nil
}

// removeHostEgressRules removes the host-side egress rules of the veth state
// names, once that veth is gone. The state is the caller's, so teardown must
// not be a way to strip a live network's filter while its veth stays up:
// only rules naming this veth come off, and only after it has been deleted.
// A veth that survived its delete keeps its rules, closed as they were.
//
// Beyond the recorded rules it removes any other rule for this veth and
// gateway: an admission whose reply never reached the orchestrator installed
// rules nobody kept, and an ACCEPT left behind would open its port to the next
// network given the same veth name.
func removeHostEgressRules(ctx context.Context, state NetworkState) {
	if state.VethHost == "" {
		return
	}
	if _, err := net.InterfaceByName(state.VethHost); err == nil {
		sandboxLog.Warn("run veth survived its teardown; leaving its egress rules in place", "veth", state.VethHost)
		return
	}
	for _, r := range state.EgressRules {
		if !argsNameVeth(r.Args, state.VethHost) {
			sandboxLog.Warn("refusing to remove an egress rule for another veth", "veth", state.VethHost, "rule", strings.Join(r.Args, " "))
			continue
		}
		if err := teardownIptables(ctx, r); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: teardown egress rule: %v\n", err)
		}
	}
	if state.HostIP != "" {
		reapEgressRules(ctx, state.HostIP, state.VethHost)
	}
}

func (hostOps) AdmitJailPorts(ctx context.Context, conversationID string, subnetIdx uint8, ports []int) (JailIngress, error) {
	if err := ValidateJailPorts(ports); err != nil {
		return JailIngress{}, err
	}
	// The network must be one this process built for this conversation and
	// has not torn down. The veth name carries only a short fragment of the
	// conversation id, so it alone could name another conversation's
	// network at the same index; the ledger holds the whole id.
	if owner, live := netnsLiveRun(NetnsNameForRun(conversationID, subnetIdx)); !live || owner != conversationID {
		return JailIngress{}, fmt.Errorf("sandbox: admit jail ports: no live run network for this conversation at subnet index %d", subnetIdx)
	}
	// And the veth must exist: a rule naming an interface that does not is
	// accepted by iptables and matches nothing.
	vethHost := vethHostName(conversationID, subnetIdx)
	if _, err := net.InterfaceByName(vethHost); err != nil {
		return JailIngress{}, fmt.Errorf("sandbox: admit jail ports: no run network %s: %w", vethHost, err)
	}
	rules, err := admitJailPorts(ctx, vethHost, hostIP(subnetIdx), ports)
	if err != nil {
		return JailIngress{}, fmt.Errorf("sandbox: %w", err)
	}
	return JailIngress{Rules: rules}, nil
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

	// Leftover runsc container state from a crashed broker. Unlike the
	// sweeps above, this one has no natural collision to rely on: the ids
	// are keyed to subnet indices a fresh boot allocates from zero in a
	// different order, so state nobody sweeps is state nothing ever reclaims.
	reapOrphanRuntimeState(ctx)

	return reapBundleOrphans(ctx)
}
