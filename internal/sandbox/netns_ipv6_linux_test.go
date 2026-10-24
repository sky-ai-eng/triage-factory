//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// hostOpsNetworkIdx is the index these tests build real networks on, below
// staleNetnsSubnetIdx for the same reason that one sits at the top.
const hostOpsNetworkIdx uint8 = 254

// requireHostOpsNetwork skips unless this process can build a whole run
// network the way the broker does: a namespace, ip6tables on a kernel with
// IPv6, and a state root on disk for the jails' /tmp.
func requireHostOpsNetwork(t *testing.T) (root string) {
	t.Helper()
	requireNetnsCapableHost(t, "tf-probe-hostops-network")
	for _, tool := range []string{"iptables", "ip6tables", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
		t.Skip("this kernel has no IPv6")
	}
	return jailTmpStateRoot(t)
}

// TestSetupNetwork_DropsIPv6FromTheJail: a jail with IPv6 of its own cannot
// reach a host listener on "::" through the host end's link-local address.
func TestSetupNetwork_DropsIPv6FromTheJail(t *testing.T) {
	const conversationID = "ipv6-drop-run"
	requireHostOpsNetwork(t)
	ctx := context.Background()

	state, err := hostOps{}.SetupNetwork(ctx, conversationID, hostOpsNetworkIdx)
	if err != nil {
		t.Fatalf("SetupNetwork: %v", err)
	}
	t.Cleanup(func() { _ = hostOps{}.TeardownNetwork(context.Background(), state) })

	// The jail side gets IPv6 back, as an unprivileged container leaves it,
	// so setup's in-netns disable cannot hide what the host drops. Where
	// /proc/sys is read-only the disable never took and this write fails too.
	_ = runIPNoErr(ctx, "netns", "exec", state.NetnsName, "sysctl", "-wq",
		"net.ipv6.conf.all.disable_ipv6=0", "net.ipv6.conf."+state.VethSandbox+".disable_ipv6=0")
	hostLL := waitLinkLocal(t, "", state.VethHost)
	waitLinkLocal(t, state.NetnsName, state.VethSandbox)

	ln, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Fatalf("listen on [::]: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	if err := dialFromNetns(state.NetnsName, state.VethSandbox, hostLL, port); err == nil {
		t.Fatal("the jail reached a host listener on [::] over IPv6")
	}

	// The control: without the drops the same dial connects, so it was the
	// drops that refused it.
	for _, chain := range ipv6DropChains {
		if err := runIP6tables(ctx, ipv6DropArgs("-D", chain, state.VethHost)...); err != nil {
			t.Fatalf("remove the %s drop: %v", chain, err)
		}
	}
	if err := dialFromNetns(state.NetnsName, state.VethSandbox, hostLL, port); err != nil {
		t.Skipf("without the drops the dial still fails (%v); this host filters IPv6 itself, so the drops cannot be shown to be what refuses it", err)
	}
}

// TestSetupNetwork_FailureRemovesWhatItBuilt: a setup that fails at its last
// step leaves no namespace, veth, ledger entry or rule behind, so a retry of
// the same run on the same index builds its network.
func TestSetupNetwork_FailureRemovesWhatItBuilt(t *testing.T) {
	const conversationID = "failed-setup-run"
	root := requireHostOpsNetwork(t)
	ctx := context.Background()
	names := runNetNames(netnsIDFrag(conversationID), hostOpsNetworkIdx)

	// A jail /tmp base with the wrong mode fails the last step of setup.
	base := filepath.Join(root, jailTmpDirName)
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}

	state, err := hostOps{}.SetupNetwork(ctx, conversationID, hostOpsNetworkIdx)
	if err == nil {
		_ = hostOps{}.TeardownNetwork(ctx, state)
		t.Fatal("SetupNetwork succeeded over a jail /tmp base it must refuse")
	}
	if state != (NetworkState{}) {
		t.Errorf("a failed SetupNetwork returned %+v; want a zero state", state)
	}
	if netnsExists(t, names.netnsName) {
		t.Error("the namespace outlived the failed setup")
	}
	if _, err := net.InterfaceByName(names.vethHost); err == nil {
		t.Error("the host veth outlived the failed setup")
	}
	if owner, live := netnsLiveRun(names.netnsName); live {
		t.Errorf("the ledger still holds the namespace for %q", owner)
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		if rules := rulesOnVeth(t, bin, names.vethHost); len(rules) > 0 {
			t.Errorf("%s rules outlived the failed setup: %q", bin, rules)
		}
	}

	if err := os.Chmod(base, jailTmpMode); err != nil {
		t.Fatal(err)
	}
	state, err = hostOps{}.SetupNetwork(ctx, conversationID, hostOpsNetworkIdx)
	if err != nil {
		t.Fatalf("SetupNetwork after a failed one, same run and index: %v", err)
	}
	if err := (hostOps{}).TeardownNetwork(ctx, state); err != nil {
		t.Fatalf("TeardownNetwork: %v", err)
	}
	if rules := rulesOnVeth(t, "ip6tables", names.vethHost); len(rules) > 0 {
		t.Errorf("ip6tables rules outlived the teardown: %q", rules)
	}
}

// waitLinkLocal returns dev's IPv6 link-local address in netns ("" for this
// process's own) once duplicate address detection has finished with it.
func waitLinkLocal(t *testing.T, netns, dev string) net.IP {
	t.Helper()
	args := []string{"-6", "-o", "addr", "show", "dev", dev, "scope", "link"}
	if netns != "" {
		args = append([]string{"-n", netns}, args...)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("ip", args...).Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				f := strings.Fields(line)
				if len(f) < 4 || f[2] != "inet6" || strings.Contains(line, "tentative") {
					continue
				}
				if ip, _, err := net.ParseCIDR(f[3]); err == nil {
					return ip
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no settled IPv6 link-local address on %s (netns %q): %s", dev, netns, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dialFromNetns opens a TCP connection to ip:port from inside netns, out of
// its interface dev. The socket is made on a thread moved into the namespace;
// the thread is never unlocked, so it exits with the goroutine rather than
// returning to the scheduler inside the namespace.
func dialFromNetns(netns, dev string, ip net.IP, port int) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		fd, err := unix.Open("/var/run/netns/"+netns, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			errc <- err
			return
		}
		defer unix.Close(fd)
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			errc <- err
			return
		}
		ifi, err := net.InterfaceByName(dev)
		if err != nil {
			errc <- err
			return
		}
		addr := net.JoinHostPort(ip.String()+"%"+strconv.Itoa(ifi.Index), strconv.Itoa(port))
		c, err := net.DialTimeout("tcp6", addr, 2*time.Second)
		if err == nil {
			_ = c.Close()
		}
		errc <- err
	}()
	return <-errc
}

// rulesOnVeth lists the INPUT and FORWARD rules bin holds that match packets
// arriving on veth.
func rulesOnVeth(t *testing.T, bin, veth string) []string {
	t.Helper()
	var rules []string
	for _, chain := range []string{"INPUT", "FORWARD"} {
		out, err := exec.Command(bin, "-t", "filter", "-S", chain).Output()
		if err != nil {
			t.Fatalf("%s -S %s: %v", bin, chain, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if argsNameVeth(strings.Fields(line), veth) {
				rules = append(rules, line)
			}
		}
	}
	return rules
}
