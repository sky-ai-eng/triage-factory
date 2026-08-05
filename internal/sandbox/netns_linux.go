//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// netnsIDFrag derives the deterministic 8-hex-char fragment embedded in a
// per-run netns name from the run id (sha1 of the runID). Extracted so the
// launch-time netns ownership check re-derives the exact same fragment
// setupNetwork used — keeping the two in lockstep by construction.
func netnsIDFrag(runID string) string {
	h := sha1.Sum([]byte(runID))
	return hex.EncodeToString(h[:])[:8]
}

// NetnsNameForRun is the per-run network-namespace name for (runID,
// subnetIdx): tf-<idFrag>-<idx>. It is deterministic in the run id, so the
// broker can re-derive the name a given run's netns MUST have and reject a
// LaunchRun that hands it any other (even a sibling run's) broker-created
// namespace — closing the netns-confusion gap where a shape-only check
// would accept any tf-shaped path. Exported so the launch validation and
// its tests build the same name this package's setup produces.
func NetnsNameForRun(runID string, subnetIdx uint8) string {
	return fmt.Sprintf("tf-%s-%d", netnsIDFrag(runID), subnetIdx)
}

// setupNetwork creates the netns + veth pair + addressing matching
// the validated recipe in docs/for-agents/specs/sky-254-runsc-validation/
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
// Returns netState with the names + IPs needed by teardown.
func setupNetwork(ctx context.Context, runID string, subnetIdx uint8) (*netState, error) {
	// Per-run identifiers. veth name length is constrained by
	// IFNAMSIZ=16, leaving 13 chars after the "vh-"/"vs-" prefix.
	//
	// idFrag MUST be hex — the reaper's regex (^tf-[0-9a-f]+-(\d+)$)
	// strict-matches hex so it can't false-positive against unrelated
	// tf-* netns owned by other processes. We can't trust the caller's
	// runID to be hex (TraceID may be "live-smoke" or any free-form
	// string), so derive a deterministic 8-hex-char fragment from a
	// sha1 of the runID. Same runID → same fragment, every time.
	idFrag := netnsIDFrag(runID)
	netnsName := NetnsNameForRun(runID, subnetIdx)
	vethHost := fmt.Sprintf("vh-%s%d", idFrag[:min(len(idFrag), 4)], subnetIdx)
	vethSandbox := fmt.Sprintf("vs-%s%d", idFrag[:min(len(idFrag), 4)], subnetIdx)
	netnsPath := "/var/run/netns/" + netnsName

	hostAddr := hostIP(subnetIdx)
	sandboxAddr := sandboxIP(subnetIdx)
	subnet := subnetCIDR(subnetIdx)

	state := &netState{
		netnsName:   netnsName,
		netnsPath:   netnsPath,
		vethHost:    vethHost,
		vethSandbox: vethSandbox,
		subnet:      subnet,
	}

	// Discover the upstream interface from the default route. Fly
	// Machines use eth0 but self-host customers may differ.
	upstreamIF, err := defaultRouteInterface(ctx)
	if err != nil {
		return nil, fmt.Errorf("netns: discover upstream interface: %w", err)
	}
	state.upstreamIF = upstreamIF

	// Delete-if-exists before the add, the same remove-first rule the per-run
	// socket and the injector cert follow. `ip netns add` fails outright on an
	// existing name, and the name is deterministic in (run id, subnet index) —
	// so a namespace leaked by a failed teardown of an earlier engagement of
	// this same run is a name collision waiting for whichever later claim
	// happens to draw the same index. The boot-time reap covers a process that
	// crashed; it does not cover a live process whose one teardown failed.
	//
	// It can never delete a live namespace: the index is allocated to this
	// bring-up before setupNetwork is called and stays held for the cell's
	// life, so no other live cell can be using a name that ends in this index.
	// Best-effort — the common case is that there is nothing to delete.
	_ = runIPNoErr(ctx, "netns", "delete", netnsName)

	// Each ip command is wrapped so failure → cleanup of partial state.
	// We return the partial state on the way out so cleanup can use it.

	steps := []struct {
		name string
		argv []string
	}{
		// netns add
		{"netns add", []string{"netns", "add", netnsName}},
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

	// Defense-in-depth: disable IPv6 inside the netns so the
	// Part B egress allowlist — which is IPv4-only (iptables, not
	// ip6tables; the runner image bundles only iptables) — can't be
	// flanked over v6. Today this is belt-and-suspenders, not an open
	// hole: the LLM proxy binds an IPv4 literal (10.42.<idx>.1), so a
	// sibling run has no v6 listener to reach, and the netns is given no
	// v6 address or route above, so there's no v6 path off-link anyway.
	// But that safety is implicit; pinning disable_ipv6 makes it safe by
	// construction, so a future dual-stack proxy or a host that
	// autoconfigures v6 on the veth can't silently reopen the
	// cross-tenant reach below the v4 filter.
	//
	// Best-effort by design (runIPNoErr swallows failure): if `sysctl`
	// is absent on some image, a hard failure here would break every
	// sandbox run for no security gain (v6 is unreachable regardless),
	// so we proceed rather than fail the run. Writing 1 to `all`
	// disables v6 on every interface in the netns (existing + future),
	// flushing any link-local the veth or lo may have picked up.
	_ = runIPNoErr(ctx, "netns", "exec", netnsName,
		"sysctl", "-wq", "net.ipv6.conf.all.disable_ipv6=1")

	return state, nil
}

// teardownNetwork is the reverse of setupNetwork. Idempotent — each
// step swallows "does not exist" errors so it's safe to call against
// a partial-init state.
func teardownNetwork(ctx context.Context, state *netState) error {
	if state == nil {
		return nil
	}
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
