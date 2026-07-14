//go:build linux && integration

// Cross-sidecar negative tests (PRCI-P4 work item 2, the live halves). Each
// stands up two real runs and proves one cannot reach the other's per-run
// isolation boundary:
//
//   - socket: from inside run B's gVisor jail, a dial of run A's agenthost
//     socket fails — B's mount namespace holds only its own run's socket. The
//     boundary is the per-run bind mount + the 0700 socket root, NOT the socket
//     file's mode (which is deliberately group-open to the shared WorktreeGID so
//     the SAME run's sandbox can connect — see socket_linux.go's Property-B
//     block). The mount-topology half (the broker rejecting a spec that names
//     another run's socket as this run's source) is already covered by
//     validate_fuzz_linux_test.go; this is the live in-sandbox dial-fails half.
//
//   - ptrace: a process at run B's credential uid cannot PTRACE_ATTACH run A's
//     sidecar. uid distinctness is derived (SidecarUID(idxA) != SidecarUID(idxB),
//     a unit assertion); the live half is the attach being denied between the two
//     running sidecars. The shared WorktreeGID supplementary group does not grant
//     this — ptrace is uid/CAP_SYS_PTRACE-gated, not group-gated.

package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// unixDialProbeArgv builds a /bin/sh payload that dials two AF_UNIX paths from
// inside the sandbox and reports REACHABLE/BLOCKED for each, mirroring the
// cross-tenant egress probe's nc pattern but for unix sockets (via python3 from
// the baked rootfs, whose connect() raises — and so exits non-zero — on a path
// that is absent or unreachable). sib is the sibling run's host socket path
// (absent inside this jail); own is DefaultSocketPath, this run's own socket.
func unixDialProbeArgv(sib string) []string {
	const probe = `import socket,sys; s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM); s.settimeout(3); s.connect(sys.argv[1])`
	script := `SIB='` + sib + `'; OWN='` + TrustedAgentHostSocketDestination + `'; ` +
		`if python3 -c '` + probe + `' "$SIB" 2>/dev/null; then echo SIBLING=REACHABLE; else echo SIBLING=BLOCKED; fi; ` +
		`if python3 -c '` + probe + `' "$OWN" 2>/dev/null; then echo OWN=REACHABLE; else echo OWN=BLOCKED; fi`
	return []string{"/bin/sh", "-c", script}
}

// TestIntegration_CrossSidecarSocketIsolation proves run B cannot dial run A's
// agenthost socket from inside its jail, while it CAN dial its own — so the
// "blocked" result is isolation, not a broken probe.
func TestIntegration_CrossSidecarSocketIsolation(t *testing.T) {
	requireRunsc(t)
	requireApk(t) // python3 for the in-sandbox unix-socket dial
	if os.Geteuid() != 0 {
		t.Skip("cross-sidecar socket isolation needs root: setuid sidecar + gVisor + socket chown")
	}

	// Give both runs' sockets a test-owned root at TrustedAgentHostSocketPath so
	// they live at the production-shaped path without touching the real,
	// root-owned /run/tf. trustedAgentHostSocketRoot is a var precisely for this.
	sockRoot := t.TempDir()
	if err := os.Chmod(sockRoot, 0o700); err != nil {
		t.Fatalf("chmod socket root: %v", err)
	}
	restore := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = sockRoot
	t.Cleanup(func() { trustedAgentHostSocketRoot = restore })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const runA, runB = "itest-sc-a", "itest-sc-b"

	// Run A is the sibling: a real live run whose agenthost socket exists on the
	// host but is NEVER mounted into run B. Its sandbox need not run a payload.
	startLiveRun(t, ctx, runA, nil)
	aSock := newAgentHostSocketListener(t, runA)

	// Run B is the prober: its own agenthost socket is served on the host and
	// bind-mounted into its jail at DefaultSocketPath; the payload dials the
	// sibling's path (must fail) and its own (must succeed).
	bSock := newAgentHostSocketListener(t, runB)
	b := startLiveRun(t, ctx, runB, func(cfg *Config) {
		cfg.Argv = unixDialProbeArgv(aSock)
		cfg.ExtraMounts = append(cfg.ExtraMounts, Mount{Source: bSock, Destination: TrustedAgentHostSocketDestination})
	})

	out, err := runToCompletion(t, b.run)
	if err != nil {
		t.Fatalf("run B probe payload: %v (output: %s)", err, out)
	}
	got := string(out)
	t.Logf("run B probe:\n  sibling(A)=%s\n  own=%s\n--- output ---\n%s", aSock, TrustedAgentHostSocketDestination, got)

	// Positive control first: if run B can't even reach its OWN socket, the probe
	// itself is broken and the "blocked sibling" result below is meaningless.
	if !strings.Contains(got, "OWN=REACHABLE") {
		t.Fatalf("positive control failed: run B could not reach its OWN agenthost socket at %s — the probe is not a valid isolation test.\noutput:\n%s", TrustedAgentHostSocketDestination, got)
	}
	if !strings.Contains(got, "SIBLING=BLOCKED") {
		t.Errorf("CROSS-SIDECAR SOCKET HOLE: run B reached run A's agenthost socket %s from inside its jail — per-run bind-mount isolation failed.\noutput:\n%s", aSock, got)
	}
}

// TestIntegration_CrossSidecarPtraceDenied proves a process at run B's
// credential uid cannot ptrace run A's sidecar — the memory-isolation boundary
// between two live per-run credential processes.
func TestIntegration_CrossSidecarPtraceDenied(t *testing.T) {
	requireRunsc(t)
	if os.Geteuid() != 0 {
		t.Skip("cross-sidecar ptrace test needs root: setuid sidecars + a probe at another band uid")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	a := startLiveRun(t, ctx, "itest-sc-a", nil)
	b := startLiveRun(t, ctx, "itest-sc-b", nil)

	// Unit half: the uids are distinct because they are derived from distinct
	// per-run subnet indices.
	if a.sidecarUID == b.sidecarUID {
		t.Fatalf("run A and run B share sidecar uid %d — derived uids are not distinct", a.sidecarUID)
	}

	pidsA := pidsAtUID(t, a.sidecarUID)
	if len(pidsA) == 0 {
		t.Fatalf("no live sidecar process at run A's uid %d to target", a.sidecarUID)
	}
	targetPid := pidsA[0]

	// Live half: attach must be denied. runPtraceProbeAsUID drops a probe to run
	// B's credential uid and tries PTRACE_ATTACH on run A's sidecar pid.
	got := runPtraceProbeAsUID(t, b.sidecarUID, targetPid)
	t.Logf("ptrace probe uid %d → run A sidecar pid %d: %s", b.sidecarUID, targetPid, got)
	if !strings.Contains(got, "PTRACE=DENIED") {
		t.Errorf("CROSS-SIDECAR MEMORY HOLE: a process at run B's uid %d attached to run A's sidecar pid %d (%q) — the per-run uid boundary does not isolate address spaces", b.sidecarUID, targetPid, got)
	}
}
