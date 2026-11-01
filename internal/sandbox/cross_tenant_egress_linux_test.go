//go:build linux && integration

// Acceptance: drive the REAL Wrap()/applyEgressPolicy path with a
// live gVisor sandbox and prove the per-sandbox egress allowlist actually
// blocks a compromised run from reaching a *sibling* run's gateway/proxy
// IP — the cross-tenant credential-abuse hole — and every port of its own
// gateway it was not admitted to, while leaving the admitted port reachable
// (no over-block).
//
// This is the gVisor half the netns-only repro couldn't cover: gVisor's
// user-space netstack writes frames straight onto the veth, so the in-netns
// OUTPUT chain (Part B layer 1) may be bypassed; the host-side veth-ingress
// filter (layer 2) is what must catch it. This test exercises the path end
// to end under real runsc.

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// acceptEcho serves a tiny "I'm here" listener: every accepted connection
// gets one byte and is closed, so a busybox `nc` probe that connects exits
// 0 (REACHABLE) and one that's dropped times out non-zero (BLOCKED).
func acceptEcho(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			_, _ = c.Write([]byte("x"))
			_ = c.Close()
		}
	}()
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v (%s)", name, strings.Join(args, " "), err, out)
	}
}

// TestIntegration_CrossTenantEgressBlocked is the Part B acceptance
// check against real gVisor.
func TestIntegration_CrossTenantEgressBlocked(t *testing.T) {
	requireRunsc(t)

	// A stand-in for "another run's gateway/proxy": a local host IP on a
	// dummy interface, NOT equal to the sandbox's own gateway. Faithful to
	// the real condition — a sibling proxy binds to a local 10.42.<M>.1 the
	// weak-host model would otherwise deliver.
	//
	// Index 250: the allocator is a [0,256) pool that hands out indices
	// ascending from 0 (subnet.go), and this test does a single Wrap, so
	// the run's own gateway lands at a low index — idx 250 is only reached
	// with 250+ concurrent live sandboxes, far beyond any integration
	// host. The sb.HostIP != sibIP guard below turns even that remote
	// collision into a clean skip rather than a confusing false pass.
	const sibIP = "10.42.250.1"
	const port = "9999"
	const unadmittedPort = "9998"
	mustRun(t, "ip", "link", "add", "tfsib0", "type", "dummy")
	defer func() { _ = exec.Command("ip", "link", "del", "tfsib0").Run() }()
	mustRun(t, "ip", "addr", "add", sibIP+"/24", "dev", "tfsib0")
	mustRun(t, "ip", "link", "set", "tfsib0", "up")

	sibLn, err := net.Listen("tcp", sibIP+":"+port)
	if err != nil {
		t.Fatalf("listen sibling %s: %v", sibIP, err)
	}
	defer sibLn.Close()
	acceptEcho(t, sibLn)

	cfg := minimalConfig(t)
	// Probe payload: read our own gateway from a file the host drops into the
	// (bind-mounted) worktree after Wrap, then probe sibling + own gateway.
	// busybox nc: exit 0 = connected (REACHABLE), non-zero = dropped/refused.
	script := `OWN=$(cat /work/own_gw); ` +
		`if printf '' | timeout 4 nc -w 2 ` + sibIP + ` ` + port + ` >/dev/null 2>&1; then echo SIBLING=REACHABLE; else echo SIBLING=BLOCKED; fi; ` +
		`if printf '' | timeout 4 nc -w 2 "$OWN" ` + port + ` >/dev/null 2>&1; then echo OWNGW=REACHABLE; else echo OWNGW=BLOCKED; fi; ` +
		`if printf '' | timeout 4 nc -w 2 "$OWN" ` + unadmittedPort + ` >/dev/null 2>&1; then echo UNADMITTED=REACHABLE; else echo UNADMITTED=BLOCKED; fi`
	cfg.Argv = []string{"/bin/sh", "-c", script}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	// Guard the documented assumption: if the allocator ever handed this
	// run the same idx our sibling stand-in occupies (only possible at
	// 250+ concurrent sandboxes), sibIP would BE the own gateway and the
	// "blocked sibling" assertion would be testing nonsense. Skip rather
	// than report a misleading pass/fail.
	if sb.HostIP == sibIP {
		t.Skipf("run gateway %s collided with sibling stand-in %s; rerun with fewer concurrent sandboxes", sb.HostIP, sibIP)
	}

	// Now that the run's gateway IP is known, (a) admit the jail to the one
	// port it may use, the way a cell admits its sidecar's listeners, (b) tell
	// the payload what the gateway is and (c) stand up listeners there so the
	// own-gateway control can succeed and the unadmitted one has something to
	// refuse. The run's own teardown removes the admitted rule with the rest.
	admitted, _ := strconv.Atoi(port)
	if err := defaultOps.AdmitJailPorts(ctx, cfg.ConversationID, sb.SubnetIdx, []int{admitted}); err != nil {
		t.Fatalf("admit jail port %s: %v", port, err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Worktree, "own_gw"), []byte(sb.HostIP+"\n"), 0o644); err != nil {
		t.Fatalf("write own_gw: %v", err)
	}
	ownLn, err := net.Listen("tcp", sb.HostIP+":"+port)
	if err != nil {
		t.Fatalf("listen own gateway %s: %v", sb.HostIP, err)
	}
	defer ownLn.Close()
	acceptEcho(t, ownLn)
	unadmittedLn, err := net.Listen("tcp", sb.HostIP+":"+unadmittedPort)
	if err != nil {
		t.Fatalf("listen own gateway %s:%s: %v", sb.HostIP, unadmittedPort, err)
	}
	defer unadmittedLn.Close()
	acceptEcho(t, unadmittedLn)

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("run payload: %v (output: %s)", err, out)
	}
	got := string(out)
	t.Logf("sandbox HostIP=%s sibling=%s\n--- probe output ---\n%s", sb.HostIP, sibIP, got)

	if !strings.Contains(got, "SIBLING=BLOCKED") {
		t.Errorf("CROSS-TENANT HOLE OPEN: sandbox reached sibling proxy IP %s — egress allowlist did not block it.\noutput:\n%s", sibIP, got)
	}
	if !strings.Contains(got, "OWNGW=REACHABLE") {
		t.Errorf("OVER-BLOCK: sandbox could not reach the admitted port on its OWN gateway %s — the allowlist is too tight (legit proxy path broken).\noutput:\n%s", sb.HostIP, got)
	}
	if !strings.Contains(got, "UNADMITTED=BLOCKED") {
		t.Errorf("HOST HOLE OPEN: sandbox reached %s:%s, a port of its gateway it was never admitted to — anything the host listens on is open to it.\noutput:\n%s", sb.HostIP, unadmittedPort, got)
	}
}
