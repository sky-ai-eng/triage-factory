//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// applyMasquerade adds the NAT POSTROUTING rule that lets the
// sandbox reach the public internet through the upstream interface.
// Matches precns-test.sh line 33:
//
//	iptables -t nat -A POSTROUTING -s <subnet> -o <upstreamIF> -j MASQUERADE
//
// Also enables net.ipv4.ip_forward (idempotent — checks current
// value first so we don't toggle on every run).
//
// Teardown finds the rule by its subnet (reapIptablesForSubnet), so
// nothing about it is returned.
func applyMasquerade(ctx context.Context, subnet, upstreamIF string) error {
	if err := ensureIPForward(); err != nil {
		return fmt.Errorf("iptables: ip_forward: %w", err)
	}

	rule := iptablesRule{
		Table: "nat",
		Chain: "POSTROUTING",
		Args: []string{
			"-s", subnet,
			"-o", upstreamIF,
			"-j", "MASQUERADE",
		},
	}
	args := append([]string{"-t", rule.Table, "-A", rule.Chain}, rule.Args...)
	return runIptables(ctx, args...)
}

// teardownIptables deletes rule, mirroring the -A or -I that installed
// it: the rollback of a policy that failed partway. Idempotent — a
// missing rule errors with exit code 1 from iptables, which we
// silence.
func teardownIptables(ctx context.Context, rule iptablesRule) error {
	if len(rule.Args) == 0 {
		return nil
	}
	args := append([]string{"-t", rule.Table, "-D", rule.Chain}, rule.Args...)
	cmd := exec.CommandContext(ctx, "iptables", args...)
	_ = cmd.Run() // best-effort
	return nil
}

// applyEgressPolicy closes a sandbox's network to everything but the
// listeners of its own credential sidecar. This closes three holes:
//
//   - cross-tenant: a compromised sandbox cannot reach a *sibling*
//     run's gateway (and thus that run's credential proxy) via the
//     shared host namespace's weak-host delivery.
//   - exfil: the sandbox cannot reach the open internet directly.
//   - the host: the sandbox cannot reach anything else the executor
//     listens on. The gateway IP is a local address of the host, so a
//     listener bound on every interface (an executor's metrics endpoint)
//     answers on it unless the gateway itself is closed.
//
// Two layers, because the sandbox runs under gVisor whose user-space
// netstack injects packets onto the veth below the in-netns netfilter
// hooks and so may not traverse the in-netns OUTPUT chain the way an
// ordinary process does:
//
//  1. In-netns OUTPUT allowlist: the gateway IP plus loopback. Proven to
//     work for ordinary-namespace traffic (the acceptance repro probes
//     from a socat process inside the netns). Vanishes with the netns —
//     no teardown needed.
//  2. Host-side ingress filter on the run's veth (vethHost), the
//     backstop that catches gVisor's netstack traffic: drop every packet
//     arriving from the sandbox, to the gateway IP and to anywhere else.
//     The sidecar's listeners are opened afterwards, port by port, by
//     admitJailPorts, once the sidecar has bound them. IPv6 arriving on
//     the veth is dropped whole (dropIPv6), and nothing reopens it.
//
// Until admitJailPorts runs the sandbox can complete no connection to the
// host at all, so a launch path that never admits fails closed. Teardown
// finds the host-side rules by veth and gateway (reapEgressRules) and the
// IPv6 drops by veth (removeIPv6Drops); the in-netns rules go with the
// netns.
func applyEgressPolicy(ctx context.Context, netnsName, vethHost, gatewayIP string) error {
	// Layer 1 — in-netns OUTPUT allowlist. Add the ACCEPTs before
	// flipping the policy to DROP (config-time, no traffic in flight,
	// but the ordering keeps the intent obvious). gVisor traffic may
	// bypass this entirely; that's fine — layer 2 is the backstop and
	// this layer is what the ordinary-namespace acceptance repro
	// exercises.
	netnsSteps := [][]string{
		{"-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-d", gatewayIP, "-j", "ACCEPT"},
		{"-P", "OUTPUT", "DROP"},
	}
	for _, args := range netnsSteps {
		if err := runIptablesInNetns(ctx, netnsName, args...); err != nil {
			return fmt.Errorf("egress: in-netns %s: %w", strings.Join(args, " "), err)
		}
	}

	// Layer 2 — host-side ingress filter on the run's veth. Every packet
	// arriving on vethHost is dropped:
	//
	//   - not to the gateway IP: local delivery to a sibling gateway
	//     (INPUT — the cross-tenant hole, since a sibling gateway is a
	//     local IP the weak-host model would otherwise accept) and
	//     forwarded internet/cross-subnet traffic (FORWARD — the exfil
	//     hole);
	//   - to the gateway IP (INPUT only; the gateway is local, so nothing
	//     addressed to it is forwarded): every port of the host, until
	//     admitJailPorts opens the sidecar's.
	//
	// The two DROPs stay separate rules, each naming the gateway, so the
	// orphan reaper can still find a leaked one by its gateway address.
	//
	// Inserted at the head of each chain (-I) so the DROPs win over any
	// pre-existing broad ACCEPT (e.g. a default-accept FORWARD policy
	// that lets the MASQUERADE'd internet path work today). The
	// "-i vethHost" match scopes them tightly to this one run's veth, so
	// head-insertion can't affect unrelated host traffic.
	var hostRules []iptablesRule
	for _, r := range []struct {
		chain string
		dest  []string
	}{
		{"INPUT", []string{"!", "-d", gatewayIP}},
		{"FORWARD", []string{"!", "-d", gatewayIP}},
		{"INPUT", []string{"-d", gatewayIP}},
	} {
		rule := iptablesRule{
			Table: "filter",
			Chain: r.chain,
			Args:  append(append([]string{"-i", vethHost}, r.dest...), "-j", "DROP"),
		}
		args := append([]string{"-t", rule.Table, "-I", rule.Chain}, rule.Args...)
		if err := runIptables(ctx, args...); err != nil {
			// Roll back any host rules already installed before failing,
			// so a partial apply doesn't leak a half-policy.
			for _, installed := range hostRules {
				_ = teardownIptables(ctx, installed)
			}
			return fmt.Errorf("egress: host-side %s: %w", r.chain, err)
		}
		hostRules = append(hostRules, rule)
	}
	if err := dropIPv6(ctx, vethHost); err != nil {
		for _, installed := range hostRules {
			_ = teardownIptables(ctx, installed)
		}
		return fmt.Errorf("egress: host-side IPv6: %w", err)
	}
	return nil
}

// ipv6DropChains are the chains dropIPv6 closes on a run's veth: INPUT for
// packets addressed to the host, FORWARD for packets routed past it.
var ipv6DropChains = []string{"INPUT", "FORWARD"}

// dropIPv6 drops every IPv6 packet arriving on vethHost. The rest of the
// host-side filter is iptables, which sees only IPv4, and the kernel gives
// both ends of a veth an IPv6 link-local address, so without these a jail
// with IPv6 of its own reaches every host listener bound to "::" through the
// host end. A kernel without IPv6 has nothing to drop and no ip6tables
// tables to write. If the second insert fails the first is removed.
func dropIPv6(ctx context.Context, vethHost string) error {
	if _, err := os.Stat("/proc/sys/net/ipv6"); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	for i, chain := range ipv6DropChains {
		if err := runIP6tables(ctx, ipv6DropArgs("-I", chain, vethHost)...); err != nil {
			for _, inserted := range ipv6DropChains[:i] {
				_ = runIP6tables(ctx, ipv6DropArgs("-D", inserted, vethHost)...)
			}
			return err
		}
	}
	return nil
}

// removeIPv6Drops removes the drops dropIPv6 inserted on veth, or with veth
// empty on every run veth, except those whose veth still exists: a live
// network keeps its rules, and a veth that survived its delete stays closed.
// The rules are found by listing the chains, so a duplicate an earlier
// network with the same veth name left goes too.
func removeIPv6Drops(ctx context.Context, veth string) {
	for _, chain := range ipv6DropChains {
		out, err := exec.CommandContext(ctx, "ip6tables", "-t", "filter", "-S", chain).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			got, ok := ipv6DropRuleVeth(line, chain)
			if !ok || (veth != "" && got != veth) {
				continue
			}
			if _, err := net.InterfaceByName(got); err == nil {
				continue
			}
			_ = runIP6tables(ctx, ipv6DropArgs("-D", chain, got)...)
		}
	}
}

func ipv6DropArgs(op, chain, veth string) []string {
	return []string{"-t", "filter", op, chain, "-i", veth, "-j", "DROP"}
}

// hostVethRE is the host-side veth name runNetNames builds.
var hostVethRE = regexp.MustCompile(`^vh-[0-9a-f]{4}[0-9]{1,3}$`)

// ipv6DropRuleVeth returns the veth an `ip6tables -S <chain>` line names
// when the line is exactly a drop dropIPv6 inserts on a run veth.
func ipv6DropRuleVeth(line, chain string) (string, bool) {
	f := strings.Fields(line)
	if len(f) != 6 || f[0] != "-A" || f[1] != chain || f[2] != "-i" || f[4] != "-j" || f[5] != "DROP" {
		return "", false
	}
	if !hostVethRE.MatchString(f[3]) {
		return "", false
	}
	return f[3], true
}

// runIP6tables wraps `ip6tables` as runIptables wraps `iptables`.
func runIP6tables(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ip6tables", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ip6tables %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// admitJailPorts opens TCP ports on a sandbox's gateway IP to its jail:
// one ACCEPT per port on the run's veth, inserted at the head of INPUT so
// it precedes the DROPs applyEgressPolicy installed. These are the only
// connections the jail can complete to the host. On failure the ones
// already installed are removed; teardown finds the rest by veth and gateway
// (reapEgressRules).
func admitJailPorts(ctx context.Context, vethHost, gatewayIP string, ports []int) error {
	var rules []iptablesRule
	for _, port := range ports {
		rule := iptablesRule{
			Table: "filter",
			Chain: "INPUT",
			Args: []string{
				"-i", vethHost,
				"-d", gatewayIP,
				"-p", "tcp", "--dport", strconv.Itoa(port),
				"-j", "ACCEPT",
			},
		}
		args := append([]string{"-t", rule.Table, "-I", rule.Chain}, rule.Args...)
		if err := runIptables(ctx, args...); err != nil {
			for _, installed := range rules {
				_ = teardownIptables(ctx, installed)
			}
			return fmt.Errorf("admit jail port %d: %w", port, err)
		}
		rules = append(rules, rule)
	}
	return nil
}

// reapIptablesForSubnet removes every POSTROUTING MASQUERADE rule
// matching the given subnet. Used by the orphan reaper at startup
// when we know a netns was leaked (because we couldn't see its Close
// run) and want to also clean the MASQUERADE rule it installed.
//
// We don't need to know the upstream interface that was used — match
// by subnet alone. Since `10.42.0.0/16` is the sandbox's private
// allocation pool, anything we find there is unambiguously ours.
//
// Best-effort: on iptables exec error, skip and move on. Returning
// errors would just turn into log spam at startup; the operator can
// `iptables -t nat -F POSTROUTING` if reaper falls behind.
func reapIptablesForSubnet(ctx context.Context, subnet string) {
	out, err := exec.CommandContext(ctx, "iptables", "-t", "nat", "-S", "POSTROUTING").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Match shape: `-A POSTROUTING -s 10.42.N.0/24 -o eth0 -j MASQUERADE`.
		if !strings.HasPrefix(line, "-A POSTROUTING ") {
			continue
		}
		if !strings.Contains(line, "-s "+subnet) {
			continue
		}
		if !strings.Contains(line, "-j MASQUERADE") {
			continue
		}
		// Swap -A for -D, exec.
		delRule := strings.Replace(line, "-A POSTROUTING", "-D POSTROUTING", 1)
		args := append([]string{"-t", "nat"}, strings.Fields(delRule)...)
		_ = exec.CommandContext(ctx, "iptables", args...).Run()
	}
}

// runIptables wraps `iptables` with context + stderr capture.
func runIptables(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "iptables", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("iptables %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runIptablesInNetns runs `ip netns exec <netnsName> iptables <args>`
// so the rule lands inside the per-run network namespace rather than
// the host root netns. Used for the in-netns OUTPUT allowlist (layer 1
// of applyEgressPolicy). Same context + stderr-capture shape as
// runIptables.
func runIptablesInNetns(ctx context.Context, netnsName string, args ...string) error {
	full := append([]string{"netns", "exec", netnsName, "iptables"}, args...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ip", full...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ip %s: %w (stderr: %s)",
			strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// reapEgressForSubnet removes leaked host-side egress rules for a given
// subnet idx (the Part B host-side layer and the ports admitJailPorts
// opened). Matches the rules by the gateway IP (10.42.<idx>.1) on a vh-
// veth in the INPUT/FORWARD chains. Mirrors reapIptablesForSubnet:
// best-effort, called from the orphan reaper when a netns was leaked by a
// hard-crashed previous TF process.
//
// A leaked host-side rule is already inert (its -i vh-<idx> interface
// was deleted with the netns, so no packet can match it), but we reap
// it for hygiene so a reused idx doesn't accrete duplicate rules — and so
// an ACCEPT left behind cannot open a port to the next run that gets the
// same veth name.
func reapEgressForSubnet(ctx context.Context, idx uint8) {
	reapEgressRules(ctx, hostIP(idx), "")
}

// reapEgressRules removes the host-side egress rules naming the gateway IP gw
// on a vh- veth. A non-empty veth narrows it to rules arriving on that veth.
func reapEgressRules(ctx context.Context, gw, veth string) {
	for _, chain := range []string{"INPUT", "FORWARD"} {
		// "-t filter" is explicit even though it's the default, to match
		// every other iptables call in this file and make clear we're
		// listing the same table applyEgressPolicy installed into.
		out, err := exec.CommandContext(ctx, "iptables", "-t", "filter", "-S", chain).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !egressRuleLineMatches(line, chain, gw) {
				continue
			}
			if veth != "" && !argsNameVeth(strings.Fields(line), veth) {
				continue
			}
			delRule := strings.Replace(line, "-A "+chain, "-D "+chain, 1)
			// strings.Fields splits the negated match into separate "!"
			// and "-d" tokens, which is exactly what iptables wants as
			// distinct argv elements (the shell-escaping headache around
			// "!" only bites when going through a shell — we exec argv
			// directly, so no quoting is involved).
			args := append([]string{"-t", "filter"}, strings.Fields(delRule)...)
			_ = exec.CommandContext(ctx, "iptables", args...).Run()
		}
	}
}

// egressRuleLineMatches reports whether an `iptables -S <chain>` line is
// one of our host-side rules for gateway IP gw: an egress DROP
// (applyEgressPolicy) or an admitted jail port (admitJailPorts). Pulled
// out as a pure function so the field-matching logic — the bug-prone part
// — is unit-testable without root or a live iptables.
//
// Field-based (not substring) matching so that:
//   - the gateway IP can't be a substring of a longer address (10.42.0.1
//     must not match 10.42.0.10), and
//   - it tolerates iptables printing the destination either as
//     "10.42.0.1" or "10.42.0.1/32" (nft vs legacy backends differ).
//
// The DROPs name the gateway either negated ("! -d <gw>", which -S prints
// with the "!" as its own token before "-d") or plain. An ACCEPT is ours
// only in the admitted shape: the plain destination and a --dport.
func egressRuleLineMatches(line, chain, gw string) bool {
	if !strings.HasPrefix(line, "-A "+chain+" ") {
		return false
	}
	fields := strings.Fields(line)
	var hasVethIn, hasDrop, hasAccept, hasGateway, gatewayNegated, hasDport bool
	for i, f := range fields {
		switch {
		case f == "-i" && i+1 < len(fields) && strings.HasPrefix(fields[i+1], "vh-"):
			hasVethIn = true
		case f == "-j" && i+1 < len(fields) && fields[i+1] == "DROP":
			hasDrop = true
		case f == "-j" && i+1 < len(fields) && fields[i+1] == "ACCEPT":
			hasAccept = true
		case f == "-d" && i+1 < len(fields) && ipFieldMatches(fields[i+1], gw):
			hasGateway = true
			gatewayNegated = i > 0 && fields[i-1] == "!"
		case f == "--dport":
			hasDport = true
		}
	}
	if !hasVethIn || !hasGateway {
		return false
	}
	return hasDrop || (hasAccept && !gatewayNegated && hasDport)
}

// argsNameVeth reports whether an iptables rule's arguments match packets
// arriving on veth ("-i <veth>").
func argsNameVeth(args []string, veth string) bool {
	for i, a := range args {
		if a == "-i" && i+1 < len(args) && args[i+1] == veth {
			return true
		}
	}
	return false
}

// ipFieldMatches reports whether an iptables address field equals ip,
// accepting both the bare "10.42.0.1" and the "10.42.0.1/32" host forms.
func ipFieldMatches(field, ip string) bool {
	return field == ip || field == ip+"/32"
}

// ensureIPForward reads /proc/sys/net/ipv4/ip_forward and writes "1"
// only if it's not already 1. Avoids fighting with other workloads
// on the host that might toggle it (Docker does too, idempotently —
// we mirror its behavior).
func ensureIPForward() error {
	const path = "/proc/sys/net/ipv4/ip_forward"
	cur, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(cur)) == "1" {
		return nil
	}
	return os.WriteFile(path, []byte("1"), 0o644)
}
