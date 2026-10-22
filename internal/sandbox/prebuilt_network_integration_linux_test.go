//go:build linux && integration

package sandbox

import (
	"context"
	stdnet "net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestIntegration_PrebuiltNetwork_LaunchAndOwnership is the real-gVisor
// validation of the netns-first split (TFAC-631): the executor path stands the
// run network up out-of-band via SetupRunNetwork, Wrap launches an agent INTO
// that network instead of allocating its own, and teardown ownership is split —
// the run's own Close must NOT reclaim the caller-owned network (the delegate's
// RunNetwork.Close does, after the sidecar exits). Skips gracefully off a
// runsc/root host.
func TestIntegration_PrebuiltNetwork_LaunchAndOwnership(t *testing.T) {
	requireRunsc(t)
	if os.Geteuid() != 0 {
		t.Skip("prebuilt-network launch needs root for netns/veth; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Stand the network up ahead of Wrap, exactly as the delegate does before
	// the pre-sandbox clone.
	net, err := SetupRunNetwork(ctx, "itest-prebuilt")
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	idx := net.Idx
	if defaultAllocator().IsFree(idx) {
		t.Fatalf("subnet idx %d should be taken after SetupRunNetwork", idx)
	}
	netnsPath := net.NetnsPath

	cfg := minimalConfig(t)
	// The network's own conversation: a launch into a network built for
	// another is refused, the jail's /tmp being derived from it.
	cfg.ConversationID = "itest-prebuilt"
	cfg.Network = net
	cfg.Argv = []string{"/bin/echo", "prebuilt-ok"}

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		_ = net.Close()
		t.Fatalf("Wrap into a caller-owned network: %v", err)
	}
	// Wrap reused the provided network — it did NOT allocate a second index.
	if sb.SubnetIdx != idx {
		t.Fatalf("Wrap used subnet idx %d, want the caller-provided %d (it must not allocate its own)", sb.SubnetIdx, idx)
	}
	if sb.HostIP != net.HostIP || sb.NetnsPath != netnsPath {
		t.Fatalf("Sandbox network fields = (%s, %s), want the provided (%s, %s)", sb.HostIP, sb.NetnsPath, net.HostIP, netnsPath)
	}

	out, err := runToCompletion(t, run)
	if err != nil {
		_ = sb.Close()
		_ = net.Close()
		t.Fatalf("run into prebuilt network: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "prebuilt-ok") {
		t.Fatalf("payload output = %q, want it to boot into the provided netns and echo", out)
	}

	// Run + sandbox teardown must leave the caller-owned network intact — the
	// veth the sidecar's proxies bound (and the subnet idx its uid derived from)
	// outlive the run; only the delegate's RunNetwork.Close reclaims them.
	_ = run.Close()
	if err := sb.Close(); err != nil {
		t.Fatalf("sb.Close: %v", err)
	}
	if defaultAllocator().IsFree(idx) {
		_ = net.Close()
		t.Fatal("sb.Close released the caller-owned subnet idx — RunNetwork.Close must own that")
	}
	if _, statErr := os.Stat(netnsPath); statErr != nil {
		_ = net.Close()
		t.Fatalf("sb.Close tore down the caller-owned netns (%s gone) — it must leave it to RunNetwork.Close", netnsPath)
	}

	// The delegate's RunNetwork.Close is what actually tears the network down
	// and releases the index back to the allocator.
	if err := net.Close(); err != nil {
		t.Fatalf("net.Close: %v", err)
	}
	if !defaultAllocator().IsFree(idx) {
		t.Fatalf("net.Close did not release subnet idx %d", idx)
	}
	if _, statErr := os.Stat(netnsPath); statErr == nil {
		t.Fatalf("netns %s still present after net.Close", netnsPath)
	}
}

// TestIntegration_RunNetwork_HostVethHasNoIPv6: the egress filter is IPv4-only,
// so the host side of the veth must refuse IPv6 outright. Otherwise the host
// gives it a link-local address and a jail with IPv6 reaches every host
// listener bound to "::" below that filter.
func TestIntegration_RunNetwork_HostVethHasNoIPv6(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("run network setup needs root for netns/veth; skipping")
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
		t.Skip("this kernel has no IPv6; nothing to disable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const conversationID = "itest-veth-ipv6"
	net, err := SetupRunNetwork(ctx, conversationID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = net.Close() })

	veth := vethHostName(conversationID, net.Idx)
	got, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + veth + "/disable_ipv6")
	if err != nil {
		t.Fatalf("read %s's IPv6 switch: %v", veth, err)
	}
	if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("IPv6 is on for the host veth %s (disable_ipv6 = %q)", veth, strings.TrimSpace(string(got)))
	}
	out, err := exec.Command("ip", "-6", "addr", "show", "dev", veth).CombinedOutput()
	if err != nil {
		t.Fatalf("ip -6 addr show dev %s: %v (%s)", veth, err, out)
	}
	if strings.Contains(string(out), "inet6") {
		t.Errorf("the host veth %s carries an IPv6 address:\n%s", veth, out)
	}
}

// TestIntegration_RunNetwork_CloseRemovesUnrecordedAdmissions: an admission
// whose reply never reached the network's owner leaves rules nobody recorded.
// Close removes them anyway, or the ACCEPT would outlive its veth and open its
// port to the next network given the same veth name.
func TestIntegration_RunNetwork_CloseRemovesUnrecordedAdmissions(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("run network setup needs root for netns/veth/iptables; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const conversationID = "itest-unrecorded-admit"
	net, err := SetupRunNetwork(ctx, conversationID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	// Straight to the privileged op, as a lost reply would leave it: the rule
	// is installed and the network's state does not know.
	if err := defaultOps.AdmitJailPorts(ctx, conversationID, net.Idx, []int{45678}); err != nil {
		_ = net.Close()
		t.Fatalf("AdmitJailPorts: %v", err)
	}
	if got := gatewayRules(t, net.HostIP); !slices.ContainsFunc(got, func(l string) bool { return strings.Contains(l, "--dport 45678") }) {
		t.Fatalf("the admitted port is not in INPUT: %v", got)
	}

	if err := net.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := gatewayRules(t, net.HostIP); len(got) != 0 {
		t.Errorf("rules for the closed network's gateway %s survived Close: %v", net.HostIP, got)
	}
}

// TestIntegration_RunNetwork_TeardownCannotOpenALiveNetwork: a teardown whose
// state names a live network's gateway, but another network's namespace and
// veth, is refused and removes nothing of the live network. The state is the
// caller's, so teardown must not be a way to strip a live network's filter
// while its veth stays up.
func TestIntegration_RunNetwork_TeardownCannotOpenALiveNetwork(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("run network setup needs root for netns/veth/iptables; skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const conversationID = "itest-live-network"
	live, err := SetupRunNetwork(ctx, conversationID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })
	before := gatewayRules(t, live.HostIP)
	if len(before) == 0 {
		t.Fatalf("the live network has no rules for its gateway %s; the check below would pass vacuously", live.HostIP)
	}

	forged := NetworkState{
		NetnsName: "tf-00000000-250",
		VethHost:  "vh-0000250",
		HostIP:    live.HostIP,
	}
	if err := defaultOps.TeardownNetwork(ctx, forged); err == nil {
		t.Error("TeardownNetwork accepted a state naming another network's gateway")
	}

	if after := gatewayRules(t, live.HostIP); !slices.Equal(after, before) {
		t.Errorf("a teardown naming another veth changed the live network's rules:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := stdnet.InterfaceByName(vethHostName(conversationID, live.Idx)); err != nil {
		t.Errorf("the live network's veth is gone: %v", err)
	}
}

// gatewayRules lists the INPUT rules naming gateway gw on a vh- veth.
func gatewayRules(t *testing.T, gw string) []string {
	t.Helper()
	out, err := exec.Command("iptables", "-t", "filter", "-S", "INPUT").Output()
	if err != nil {
		t.Fatalf("iptables -S INPUT: %v", err)
	}
	var rules []string
	for _, line := range strings.Split(string(out), "\n") {
		if egressRuleLineMatches(line, "INPUT", gw) {
			rules = append(rules, line)
		}
	}
	return rules
}
