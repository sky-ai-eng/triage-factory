//go:build linux && integration

package sandbox

import (
	"context"
	"os"
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
