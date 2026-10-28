//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// netnsIDFrag derives the deterministic 8-hex-char fragment embedded in a
// per-run netns name from the run id (sha1 of the conversationID). Extracted so the
// launch-time netns ownership check re-derives the exact same fragment
// setupNetwork used — keeping the two in lockstep by construction.
func netnsIDFrag(conversationID string) string {
	h := sha1.Sum([]byte(conversationID))
	return hex.EncodeToString(h[:])[:8]
}

// NetnsNameForRun is the per-run network-namespace name for (conversationID,
// subnetIdx): tf-<idFrag>-<idx>. It is deterministic in the run id, so the
// broker can re-derive the name a given run's netns MUST have and reject a
// LaunchRun that hands it any other (even a sibling run's) broker-created
// namespace — closing the netns-confusion gap where a shape-only check
// would accept any tf-shaped path. Exported so the launch validation and
// its tests build the same name this package's setup produces.
func NetnsNameForRun(conversationID string, subnetIdx uint8) string {
	return runNetNames(netnsIDFrag(conversationID), subnetIdx).netnsName
}

// runNetNames is every name of the run network at subnetIdx for a run id
// hashing to idFrag. setupNetwork builds from it, and TeardownNetwork accepts
// only a state that names exactly what it gives, so a teardown can delete
// nothing a setup would not have built.
func runNetNames(idFrag string, subnetIdx uint8) *netState {
	netnsName := fmt.Sprintf("tf-%s-%d", idFrag, subnetIdx)
	short := idFrag[:min(len(idFrag), 4)]
	return &netState{
		netnsName:   netnsName,
		netnsPath:   "/var/run/netns/" + netnsName,
		vethHost:    fmt.Sprintf("vh-%s%d", short, subnetIdx),
		vethSandbox: fmt.Sprintf("vs-%s%d", short, subnetIdx),
		subnet:      subnetCIDR(subnetIdx),
	}
}

// runNetnsRE is the namespace name runNetNames builds: an id fragment of
// exactly netnsIDFrag's length and a decimal subnet index.
var runNetnsRE = regexp.MustCompile(`^tf-([0-9a-f]{8})-([0-9]{1,3})$`)

// runNetFromNetnsName inverts runNetNames: the names of the run network whose
// namespace is name, and its subnet index, if name is one runNetNames builds.
func runNetFromNetnsName(name string) (*netState, uint8, bool) {
	m := runNetnsRE.FindStringSubmatch(name)
	if m == nil {
		return nil, 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n > 255 {
		return nil, 0, false
	}
	names := runNetNames(m[1], uint8(n))
	if names.netnsName != name {
		// A non-canonical index such as "007" names no namespace setup builds.
		return nil, 0, false
	}
	return names, uint8(n), true
}

// setupNetwork creates the netns + veth pair + addressing matching
// the validated recipe in specs/sky-254-runsc-validation/
// precns-test.sh (lines 7-33). Shells out to `ip` rather than using
// netlink Go bindings because:
//
//   - The TF runner image bundles iproute2 anyway, so no
//     new dep weight.
//   - Matches the probe verbatim, making it trivial to cross-check
//     by hand and debug from `ip` man pages.
//   - Per-run overhead is single-digit milliseconds vs ~80ms gVisor
//     cold-start, so the speed difference doesn't matter.
//
// Returns netState with the names + IPs needed by teardown, with the error
// too once the namespace exists, and nil before it does.
func setupNetwork(ctx context.Context, conversationID string, subnetIdx uint8) (*netState, error) {
	// Per-run identifiers. veth name length is constrained by
	// IFNAMSIZ=16, leaving 13 chars after the "vh-"/"vs-" prefix.
	//
	// idFrag MUST be hex — the reaper's regex (^tf-[0-9a-f]+-(\d+)$)
	// strict-matches hex so it can't false-positive against unrelated
	// tf-* netns owned by other processes. We can't trust the caller's
	// conversationID to be hex (TraceID may be "live-smoke" or any free-form
	// string), so derive a deterministic 8-hex-char fragment from a
	// sha1 of the conversationID. Same conversationID → same fragment, every time.
	state := runNetNames(netnsIDFrag(conversationID), subnetIdx)
	netnsName := state.netnsName
	vethHost := state.vethHost
	vethSandbox := state.vethSandbox

	hostAddr := hostIP(subnetIdx)
	sandboxAddr := sandboxIP(subnetIdx)

	// Discover the upstream interface from the default route. Fly
	// Machines use eth0 but self-host customers may differ.
	upstreamIF, err := defaultRouteInterface(ctx)
	if err != nil {
		return nil, fmt.Errorf("netns: discover upstream interface: %w", err)
	}
	state.upstreamIF = upstreamIF

	// Reclaim a stale namespace of this name before the add, the same
	// remove-first rule the per-run socket and the injector cert follow.
	// `ip netns add` fails outright on an existing name, and the name is
	// deterministic in (run id, subnet index) — so a namespace leaked by a
	// failed teardown of an earlier engagement of this same run is a name
	// collision waiting for whichever later claim happens to draw the same
	// index. The boot-time reap covers a process that crashed; it does not
	// cover a live process whose one teardown failed.
	//
	// Gated on this process's OWN ledger, not on the caller's word. The
	// argument that this name cannot belong to a live cell — the subnet index
	// is held for the cell's life, so no other live cell can be using a name
	// ending in it — is a property of the orchestrator's in-process allocator,
	// and the run id and index reach this function as unvalidated RPC
	// arguments. Every other remove-first here is bounded by something the
	// kernel enforces; a namespace has no owner to check, so nothing but this
	// gate stands between a caller with a wrong idea of what is stale and a
	// live cell's namespace.
	//
	// What that costs is name theft rather than an outage, and the gate is
	// worth having anyway. `ip netns delete` unlinks the NAME; the namespace
	// itself is refcounted by the processes inside it, so the victim keeps its
	// interfaces and its traffic keeps flowing. What breaks is the bookkeeping
	// on top: the name now resolves to a namespace the victim is not in, and
	// the victim's own teardown — which works by name — later unlinks the
	// successor's instead, and so on down the chain. Silent cross-cell state
	// corruption is not a trade worth making for a reclaim, so a namespace this
	// process created and has not torn down is left exactly where it is, and
	// the add below fails loudly as it always did.
	if owner, live := netnsLiveRun(netnsName); live {
		sandboxLog.Error("refusing to reclaim a network namespace this process created and has not torn down; the launch will fail rather than take a live cell's namespace name",
			"netns", netnsName, "live_run", owner, "requested_run", conversationID, "subnet_idx", subnetIdx)
	} else {
		// Best-effort — the common case is that there is nothing to delete.
		_ = runIPNoErr(ctx, "netns", "delete", netnsName)
	}

	// The namespace itself, ahead of the rest, so the ledger records it the
	// moment it exists rather than after the addressing that follows. A
	// failure from here on returns the state, which the caller tears down,
	// and the teardown drops the ledger entry first.
	//
	// A failed add returns no state: the name may be a live cell's, which the
	// gate above left in place, and a teardown by name would remove that
	// cell's network.
	if err := runIP(ctx, "netns", "add", netnsName); err != nil {
		return nil, fmt.Errorf("netns: netns add: %w", err)
	}
	markNetnsLive(netnsName, conversationID)

	// Each ip command is wrapped so failure → cleanup of partial state.
	// We return the partial state on the way out so cleanup can use it.

	steps := []struct {
		name string
		argv []string
	}{
		// veth pair
		{"veth add", []string{"link", "add", vethHost, "type", "veth", "peer", "name", vethSandbox}},
		// move sandbox side into netns
		{"veth set ns", []string{"link", "set", vethSandbox, "netns", netnsName}},
		// host side: address + up
		{"host addr", []string{"addr", "add", hostAddr + "/24", "dev", vethHost}},
		{"host up", []string{"link", "set", vethHost, "up"}},
		// sandbox side: address + up (inside netns)
		{"sandbox addr", []string{"-n", netnsName, "addr", "add", sandboxAddr + "/24", "dev", vethSandbox}},
		{"sandbox up", []string{"-n", netnsName, "link", "set", vethSandbox, "up"}},
		{"sandbox lo up", []string{"-n", netnsName, "link", "set", "lo", "up"}},
		// default route in sandbox
		{"sandbox default route", []string{"-n", netnsName, "route", "add", "default", "via", hostAddr}},
	}
	for _, step := range steps {
		if err := runIP(ctx, step.argv...); err != nil {
			return state, fmt.Errorf("netns: %s: %w", step.name, err)
		}
	}

	// IPv6 off in the netns, so the jail has no IPv6 address to send from.
	// Best-effort (runIPNoErr swallows failure): it needs `sysctl` on the
	// host and a writable /proc/sys, which an unprivileged container does
	// not have, and the host-side IPv6 drops (dropIPv6, applied with the
	// egress policy) are the enforced half. Writing 1 to `all` disables v6
	// on every interface in the netns (existing + future), flushing any
	// link-local the veth or lo may have picked up.
	_ = runIPNoErr(ctx, "netns", "exec", netnsName,
		"sysctl", "-wq", "net.ipv6.conf.all.disable_ipv6=1")

	return state, nil
}

// ArmJailNetwork puts the jail side of a run network back the way
// setupNetwork left it: the netns veth's address, its link up, and the default
// route through the gateway. runsc's sandbox networking moves the address and
// route off the netns into the jail's own stack when a jail starts, and does
// not put them back when it exits, so a second jail launched into the same
// network would find nothing to configure and reach nothing. The broker runs
// it before every jail launch; it changes nothing for the first.
//
// netnsPath is validated as a launch's is, and the network must be one this
// process built for conversationID and has not torn down: every name and
// address is derived from the pair, none taken from the caller.
func ArmJailNetwork(ctx context.Context, conversationID, netnsPath string) error {
	idx, err := validateNetnsPath(conversationID, netnsPath)
	if err != nil {
		return err
	}
	names := runNetNames(netnsIDFrag(conversationID), idx)
	if owner, live := netnsLiveRun(names.netnsName); !live || owner != conversationID {
		return fmt.Errorf("sandbox: arm jail network: no live run network for this conversation at subnet index %d", idx)
	}
	steps := []struct {
		name string
		argv []string
	}{
		{"sandbox addr", []string{"-n", names.netnsName, "addr", "replace", sandboxIP(idx) + "/24", "dev", names.vethSandbox}},
		{"sandbox up", []string{"-n", names.netnsName, "link", "set", names.vethSandbox, "up"}},
		{"sandbox default route", []string{"-n", names.netnsName, "route", "replace", "default", "via", hostIP(idx)}},
	}
	for _, step := range steps {
		if err := runIP(ctx, step.argv...); err != nil {
			return fmt.Errorf("sandbox: arm jail network: %s: %w", step.name, err)
		}
	}
	return nil
}

// teardownNetwork is the reverse of setupNetwork. Idempotent — each
// step swallows "does not exist" errors so it's safe to call against
// a partial-init state.
func teardownNetwork(ctx context.Context, state *netState) error {
	if state == nil {
		return nil
	}
	// Drop the ledger entry first, and unconditionally. A teardown whose
	// `ip netns delete` fails is exactly how a namespace leaks, and that leak
	// is what a later bring-up has to be free to reclaim — keeping a failed
	// teardown "live" would wedge this (run id, index) pair until the process
	// restarted, which is the opposite of what the reclaim exists for.
	forgetNetns(state.netnsName)
	// `ip link delete` on the host veth also removes its peer in
	// the netns (kernel keeps the pair atomic).
	_ = runIPNoErr(ctx, "link", "delete", state.vethHost)
	// `ip netns delete` unmounts /var/run/netns/<name> + removes
	// the file.
	_ = runIPNoErr(ctx, "netns", "delete", state.netnsName)
	return nil
}

// runIP wraps `ip` with context + stderr capture. Returns an error
// whose message includes the ip stderr output for debuggability.
func runIP(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w (output: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runIPNoErr wraps runIP for cleanup paths that need "best-effort,
// don't fail loudly if already gone."
func runIPNoErr(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ip", args...)
	_ = cmd.Run() // ignore — cleanup is best-effort
	return nil
}

// defaultRouteInterface returns the interface name carrying the
// default route. Matches probe line 31's awk pattern:
//
//	ip route show default | awk '/default/ {for (i=1;i<=NF;i++)
//	  if ($i == "dev") {print $(i+1); exit}}'
//
// Implemented in Go so we don't need an awk dep + so failures are
// reported clearly.
func defaultRouteInterface(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "ip", "route", "show", "default")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				return fields[i+1], nil
			}
		}
	}
	return "", fmt.Errorf("no default route found")
}

// netState collects per-run network identifiers for the teardown
// path. Stored on Sandbox.teardown.
type netState struct {
	netnsName   string
	netnsPath   string
	vethHost    string
	vethSandbox string
	subnet      string
	upstreamIF  string
}

// min is a local helper to avoid importing constraints for one use.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// vethHostName is the host-side veth of conversationID's network at
// subnetIdx. Derived, like every per-run network name, so a privileged op
// that needs it (AdmitJailPorts) re-derives it rather than trusting a name
// it is handed.
func vethHostName(conversationID string, subnetIdx uint8) string {
	return runNetNames(netnsIDFrag(conversationID), subnetIdx).vethHost
}
