//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
// Returns the rule's literal args so teardownIptables can mirror
// them in a -D call.
func applyMasquerade(ctx context.Context, subnet, upstreamIF string) (iptablesRule, error) {
	if err := ensureIPForward(); err != nil {
		return iptablesRule{}, fmt.Errorf("iptables: ip_forward: %w", err)
	}

	rule := iptablesRule{
		table: "nat",
		chain: "POSTROUTING",
		args: []string{
			"-s", subnet,
			"-o", upstreamIF,
			"-j", "MASQUERADE",
		},
	}
	args := append([]string{"-t", rule.table, "-A", rule.chain}, rule.args...)
	if err := runIptables(ctx, args...); err != nil {
		return iptablesRule{}, err
	}
	return rule, nil
}

// teardownIptables removes the masquerade rule. Idempotent — a
// missing rule errors with exit code 1 from iptables, which we
// silence.
func teardownIptables(ctx context.Context, rule iptablesRule) error {
	if len(rule.args) == 0 {
		return nil
	}
	args := append([]string{"-t", rule.table, "-D", rule.chain}, rule.args...)
	cmd := exec.CommandContext(ctx, "iptables", args...)
	_ = cmd.Run() // best-effort
	return nil
}

// applyEgressPolicy restricts a sandbox's egress to its own gateway IP
// (gatewayIP, = 10.42.<idx>.1) plus loopback, default-dropping
// everything else. This closes two holes at once (SKY-395):
//
//   - cross-tenant: a compromised sandbox can no longer reach a
//     *sibling* run's gateway (and thus that run's credential proxy)
//     via the shared host namespace's weak-host delivery.
//   - exfil: the sandbox can no longer reach the open internet
//     directly; its only legitimate destinations are the per-run
//     proxies bound on its own gateway IP.
//
// IP-scoped, not port-scoped: multiple proxies bind to the gateway IP
// (LLM now, the git proxy in SKY-333 later), so the whole gateway IP is
// allowed rather than a single port.
//
// Two layers, because the sandbox runs under gVisor whose user-space
// netstack injects packets onto the veth below the in-netns netfilter
// hooks and so may not traverse the in-netns OUTPUT chain the way an
// ordinary process does:
//
//  1. In-netns OUTPUT allowlist. Proven to work for ordinary-namespace
//     traffic (the SKY-395 acceptance repro probes from a socat process
//     inside the netns). Vanishes with the netns — no teardown needed.
//  2. Host-side ingress filter on the run's veth (vethHost): drop any
//     packet arriving from the sandbox whose destination is not the
//     gateway IP. This is the backstop that catches gVisor's netstack
//     traffic. Returned for teardown, mirroring applyMasquerade.
//
// Own-gateway traffic is excluded from the drop ("! -d gatewayIP") in
// both layers, so the legitimate proxy hop is never affected. Returns
// the host-side rules so Close can remove them; the in-netns rules need
// no teardown.
func applyEgressPolicy(ctx context.Context, netnsName, vethHost, gatewayIP string) ([]iptablesRule, error) {
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
			return nil, fmt.Errorf("egress: in-netns %s: %w", strings.Join(args, " "), err)
		}
	}

	// Layer 2 — host-side ingress filter on the run's veth. Drop every
	// packet arriving on vethHost whose destination is not the gateway
	// IP: that covers both local delivery to a sibling gateway (INPUT —
	// the cross-tenant hole, since a sibling gateway is a local IP the
	// weak-host model would otherwise accept) and forwarded
	// internet/cross-subnet traffic (FORWARD — the exfil hole).
	//
	// Inserted at the head of each chain (-I) so the DROP wins over any
	// pre-existing broad ACCEPT (e.g. a default-accept FORWARD policy
	// that lets the MASQUERADE'd internet path work today). The
	// "-i vethHost" match scopes it tightly to this one run's veth, so
	// head-insertion can't affect unrelated host traffic.
	var hostRules []iptablesRule
	for _, chain := range []string{"INPUT", "FORWARD"} {
		rule := iptablesRule{
			table: "filter",
			chain: chain,
			args: []string{
				"-i", vethHost,
				"!", "-d", gatewayIP,
				"-j", "DROP",
			},
		}
		args := append([]string{"-t", rule.table, "-I", rule.chain}, rule.args...)
		if err := runIptables(ctx, args...); err != nil {
			// Roll back any host rules already installed before failing,
			// so a partial apply doesn't leak a half-policy.
			for _, r := range hostRules {
				_ = teardownIptables(ctx, r)
			}
			return nil, fmt.Errorf("egress: host-side %s: %w", chain, err)
		}
		hostRules = append(hostRules, rule)
	}
	return hostRules, nil
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

// reapEgressForSubnet removes leaked host-side egress DROP rules for a
// given subnet idx (the SKY-395 Part B host-side layer). Matches the
// rules by the gateway IP (10.42.<idx>.1) on a vh- veth in the
// INPUT/FORWARD chains. Mirrors reapIptablesForSubnet: best-effort,
// called from the orphan reaper when a netns was leaked by a
// hard-crashed previous TF process.
//
// A leaked host-side rule is already inert (its -i vh-<idx> interface
// was deleted with the netns, so no packet can match it), but we reap
// it for hygiene so a reused idx doesn't accrete duplicate rules.
func reapEgressForSubnet(ctx context.Context, idx uint8) {
	gw := hostIP(idx)
	for _, chain := range []string{"INPUT", "FORWARD"} {
		out, err := exec.CommandContext(ctx, "iptables", "-S", chain).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !egressRuleLineMatches(line, chain, gw) {
				continue
			}
			delRule := strings.Replace(line, "-A "+chain, "-D "+chain, 1)
			args := append([]string{"-t", "filter"}, strings.Fields(delRule)...)
			_ = exec.CommandContext(ctx, "iptables", args...).Run()
		}
	}
}

// egressRuleLineMatches reports whether an `iptables -S <chain>` line is
// one of our host-side egress DROP rules for gateway IP gw. Pulled out
// as a pure function so the field-matching logic — the bug-prone part —
// is unit-testable without root or a live iptables.
//
// Field-based (not substring) matching so that:
//   - the gateway IP can't be a substring of a longer address (10.42.0.1
//     must not match 10.42.0.10), and
//   - it tolerates iptables printing the destination either as
//     "10.42.0.1" or "10.42.0.1/32" (nft vs legacy backends differ).
//
// The destination in our rule is negated ("! -d <gw>"); -S prints the
// "!" as its own token before "-d", so scanning for "-d" still finds
// the address field that follows it.
func egressRuleLineMatches(line, chain, gw string) bool {
	if !strings.HasPrefix(line, "-A "+chain+" ") {
		return false
	}
	fields := strings.Fields(line)
	var hasVethIn, hasDrop, hasGateway bool
	for i, f := range fields {
		switch {
		case f == "-i" && i+1 < len(fields) && strings.HasPrefix(fields[i+1], "vh-"):
			hasVethIn = true
		case f == "-j" && i+1 < len(fields) && fields[i+1] == "DROP":
			hasDrop = true
		case f == "-d" && i+1 < len(fields) && ipFieldMatches(fields[i+1], gw):
			hasGateway = true
		}
	}
	return hasVethIn && hasDrop && hasGateway
}

// ipFieldMatches reports whether an iptables address field equals ip,
// accepting both the bare "10.42.0.1" and the "10.42.0.1/32" host forms.
func ipFieldMatches(field, ip string) bool {
	return field == ip || field == ip+"/32"
}

// ensureIPForward reads /proc/sys/net/ipv4/ip_forward and writes "1"
// only if it's not already 1. Avoids fighting with other workloads
// on the host that might toggle it (Docker does too, idempotently —
// we mirror its behaviour).
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
