//go:build linux && integration

// Live two-run sidecar harness for the PRCI-P4 hardening suite. It stands up
// the first end-to-end broker-spawned credential sidecar under runsc that the
// package has: production always launches the run-sidecar through the cap-broker
// (a separate process this package can't import without a cycle), so — exactly
// as integration_linux_test.go does for the run launch — this installs an
// in-process launcher built from the same LaunchSidecarProcess primitive the
// broker uses, and drives LaunchSidecar end to end.
//
// One harness (startLiveRun) backs three negatives that all need real per-run
// artifacts rather than a stand-in: the eviction proof (eviction_linux_test.go,
// which reads the sidecar's per-run uid band out of /proc) and the cross-sidecar
// socket + memory negatives (cross_sidecar_negatives_linux_test.go). Each run
// gets a real gVisor sandbox (Wrap → real netns/subnet index) and a real
// credential-sidecar process at SidecarUID(SubnetIdx).

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/procname"
)

// inProcessSidecarLauncher is the integration suite's stand-in for the
// cap-broker's sidecar launch, the sidecar analog of inProcessLauncher. It runs
// the same LaunchSidecarProcess primitive (a real setuid+exec with the stdio
// handed across a socketpair) IN THIS process, so LaunchSidecar exercises its
// real translation path — SidecarConfig → SidecarUID(subnetIdx) →
// ValidateSidecarLaunchParams → the setuid exec — with the only difference from
// production being the absence of a broker process boundary. Test-only.
type inProcessSidecarLauncher struct{}

func (inProcessSidecarLauncher) LaunchSidecar(ctx context.Context, p SidecarLaunchParams) (LaunchedSidecar, error) {
	// Gate the params exactly where the broker does, so the in-process path
	// refuses an out-of-band uid the same way production would.
	if err := ValidateSidecarLaunchParams(p); err != nil {
		return nil, err
	}

	// A connected AF_UNIX pair standing in for the broker's socket-by-path stdio
	// handoff: fds[0] becomes the sidecar's stdin+stdout, fds[1] the
	// orchestrator's supervision end.
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("sandbox: sidecar socketpair: %w", err)
	}
	childFile := os.NewFile(uintptr(fds[0]), "sidecar-stdio")
	orchFile := os.NewFile(uintptr(fds[1]), "orch-stdio")
	conn, err := net.FileConn(orchFile)
	_ = orchFile.Close() // FileConn dups; drop our copy
	if err != nil {
		_ = childFile.Close()
		return nil, fmt.Errorf("sandbox: wrap sidecar supervision socket: %w", err)
	}

	var stderr testSyncBuf
	// No extra descriptors: production's broker also binds the run's
	// shared-origin listener and passes it here, but that is a grant for the gh
	// channel, not part of the sidecar lifecycle this harness exercises. A
	// sidecar handed none falls back to an ephemeral port for its injector, so
	// omitting it changes nothing about what is under test.
	rt, err := LaunchSidecarProcess(ctx, p.ContainerID, p.UID, p.GID, childFile, nil, &stderr)
	// LaunchSidecarProcess closed its own copy of childFile.
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sandbox: launch in-process sidecar: %w (stderr: %s)", err, stderr.String())
	}
	return &inProcessSidecar{rt: rt, conn: conn}, nil
}

// inProcessSidecar is the LaunchedSidecar the test launcher returns.
type inProcessSidecar struct {
	rt        *SidecarRuntime
	conn      net.Conn
	closeOnce sync.Once
}

var _ LaunchedSidecar = (*inProcessSidecar)(nil)

func (s *inProcessSidecar) Supervision() net.Conn { return s.conn }

// Close SIGKILLs the sidecar AND reaps it. Reaping (Wait) is load-bearing for
// the eviction proof: a killed-but-unreaped process is a zombie whose
// /proc/<pid>/status — including its Uid — stays readable, which would make the
// uid-band-vacates assertion never fire. Mirrors sidecarHandle.Close's
// kill-then-drain, minus the broker round trip.
func (s *inProcessSidecar) Close() error {
	s.closeOnce.Do(func() {
		_ = s.rt.Kill()
		_, _ = s.rt.Wait()
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
	return nil
}

// --- re-exec test-child intercepts (driven from the integration TestMain) ---

// maybeRunSidecarSubcommand makes a re-exec'd test binary behave as a real,
// blocking run-sidecar when LaunchSidecarProcess execs it with the fixed
// run-sidecar argv (the broker's own command). It wears the sidecar's kernel
// comm and idles at its per-run uid until SIGKILLed by Close — a faithful
// stand-in for the real sidecar for the eviction / cross-sidecar negatives,
// which observe it by uid, not by the credential protocol it would otherwise
// serve. Must run before the suite's own TestMain setup, so the child never
// pre-warms the rootfs or runs tests.
func maybeRunSidecarSubcommand() {
	if len(os.Args) > 1 && os.Args[1] == sidecarSubcommand {
		procname.SetTitle(SidecarCommName)
		select {} // until SIGKILLed by inProcessSidecar.Close
	}
}

// ptraceProbeEnv, when set to a target pid, makes a re-exec'd test binary
// attempt a PTRACE_ATTACH on that pid and report the outcome on stdout. Spawned
// at another run's sidecar uid to prove the memory-isolation boundary.
const ptraceProbeEnv = "TF_TEST_PTRACE_TARGET_PID"

// maybeRunPtraceProbe is the ptraceProbeEnv child body. It prints exactly one
// PTRACE=... line and exits 0 regardless of outcome, so the parent reads the
// result from stdout rather than an exit code.
func maybeRunPtraceProbe() {
	target := os.Getenv(ptraceProbeEnv)
	if target == "" {
		return
	}
	pid, err := strconv.Atoi(target)
	if err != nil {
		fmt.Fprintln(os.Stdout, "PTRACE=BADPID")
		os.Exit(0)
	}
	// PTRACE_ATTACH binds the tracee to the calling thread; lock it so a
	// scheduler hop can't move the syscall off the thread that must own the
	// trace relationship.
	runtime.LockOSThread()
	if err := syscall.PtraceAttach(pid); err != nil {
		fmt.Fprintf(os.Stdout, "PTRACE=DENIED:%v\n", err)
		os.Exit(0)
	}
	// Reached only if isolation is broken. Detach best-effort so the target
	// isn't left stopped, then report the break.
	_ = syscall.PtraceDetach(pid)
	fmt.Fprintln(os.Stdout, "PTRACE=OK")
	os.Exit(0)
}

// sidecarHarnessErr records a failure from installInProcessSidecarHarness (set
// once by TestMain). startLiveRun checks it first so a broken harness fails the
// live-sidecar tests with the exact cause rather than a cryptic setuid-exec
// error — and the non-sidecar integration tests, which don't touch it, still run.
var sidecarHarnessErr error

// installInProcessSidecarHarness wires the sidecar-launch seam for the
// integration TestMain: it points sidecarBinaryPath at a world-traversable copy
// of this test binary and installs the in-process sidecar launcher. The copy is
// needed because the real test binary lives under `go test`'s 0700 build cache,
// which a setuid-to-sidecar-uid exec cannot traverse (the kernel resolves the
// path after the credential switch) — the same constraint startSidecarCommHelper
// and withStubSidecarBinary already work around.
//
// All-or-nothing: the two seams (sidecarBinaryPath, sidecarLauncher) are wired
// together only AFTER the accessible copy exists, so a setup failure never
// leaves the launcher installed while sidecarBinaryPath still points at the
// un-exec'able 0700 build-cache binary — that partial state turns an
// environment problem into a cryptic setuid-exec EACCES deep inside a test.
// Returns the (non-nil) cleanup for whatever it created, plus any setup error;
// on error neither seam is wired, and startLiveRun surfaces the error directly.
func installInProcessSidecarHarness() (func(), error) {
	dir, err := os.MkdirTemp("", "tf-sidecar-harness-")
	if err != nil {
		return func() {}, fmt.Errorf("mkdtemp: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o755); err != nil {
		return cleanup, fmt.Errorf("chmod harness dir: %w", err)
	}
	binCopy := filepath.Join(dir, "tf-sidecar-testbin")
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		return cleanup, fmt.Errorf("read test binary: %w", err)
	}
	if err := os.WriteFile(binCopy, data, 0o755); err != nil {
		return cleanup, fmt.Errorf("write binary copy: %w", err)
	}
	sidecarBinaryPath = func() (string, error) { return binCopy, nil }
	sidecarLauncher = inProcessSidecarLauncher{}
	return cleanup, nil
}

// --- the two-run harness ---

// liveRun bundles the two real live artifacts one run owns for this suite: its
// gVisor sandbox (Wrap) and its credential-sidecar process (LaunchSidecar at
// SidecarUID(SubnetIdx)). The sandbox is Wrapped but not Started by startLiveRun
// — the eviction and memory negatives need only the sidecar process and the
// run's subnet index; the socket negative Starts run B's sandbox itself with a
// probe payload.
type liveRun struct {
	conversationID string
	run            LaunchedRun
	sb             *Sandbox
	sidecar        LaunchedSidecar
	sidecarUID     int
}

// startLiveRun Wraps a sandbox and launches its credential sidecar, registering
// a teardown that honors the documented ordering: the sidecar is killed BEFORE
// the run's subnet index is released (sb.Close), or a concurrent run could be
// handed the same uid while this one's sidecar is still exiting
// (LaunchSidecar's own doc warns this). customize, if non-nil, mutates the
// Config before Wrap (the socket negative uses it to set run B's probe argv and
// its own-socket bind mount).
func startLiveRun(t *testing.T, ctx context.Context, conversationID string, customize func(cfg *Config)) *liveRun {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("live sidecar harness needs root: setuid sidecar + gVisor caps")
	}
	requireRunsc(t)
	if sidecarHarnessErr != nil {
		t.Fatalf("live sidecar harness was not wired at startup: %v", sidecarHarnessErr)
	}

	cfg := minimalConfig(t)
	cfg.ConversationID = conversationID
	if customize != nil {
		customize(&cfg)
	}

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap %s: %v", conversationID, err)
	}

	sidecar, err := LaunchSidecar(ctx, SidecarConfig{ConversationID: conversationID, SubnetIdx: sb.SubnetIdx})
	if err != nil {
		_ = run.Close()
		_ = sb.Close()
		t.Fatalf("LaunchSidecar %s (subnet idx %d): %v", conversationID, sb.SubnetIdx, err)
	}

	lr := &liveRun{conversationID: conversationID, run: run, sb: sb, sidecar: sidecar, sidecarUID: SidecarUID(sb.SubnetIdx)}
	t.Cleanup(lr.teardown)
	return lr
}

// teardown closes the run's artifacts in the load-bearing order: sidecar first
// (frees the per-run uid), then the run, then the sandbox (releases the subnet
// index the uid is derived from). Idempotent via the handles' own once-guards.
func (lr *liveRun) teardown() {
	if lr.sidecar != nil {
		_ = lr.sidecar.Close()
	}
	if lr.run != nil {
		_ = lr.run.Close()
	}
	if lr.sb != nil {
		_ = lr.sb.Close()
	}
}

// --- /proc sidecar observation (the eviction + memory proofs read this) ---

// sidecarPidsAtUID returns the pids that are BOTH a tf-sidecar (comm ==
// SidecarCommName) AND running at the given real uid — the exact pair
// reapOrphanSidecars keys on, and for the same reason: a uid match alone would
// miscount an unrelated host process that happens to land in the reserved
// sidecar band (mis-asserting "not evicted", or handing the ptrace probe the
// wrong target), while a comm match alone can't tell one run's sidecar from
// another's. The comm scopes to "is a TF sidecar"; the uid scopes to "this
// specific run" (SidecarUID(idx) is unique per run). Since the sidecar holds the
// run's whole credential set in one process and forks no credential-bearing
// child (the node agent runs inside the gVisor jail, not under the sidecar),
// "no such process" is "this run's credential address space is gone" — and it
// needs no production Pid() accessor on the sidecar handle.
func sidecarPidsAtUID(t *testing.T, uid int) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue // exited mid-scan, or unreadable — not ours
		}
		if strings.TrimRight(string(comm), "\n") != SidecarCommName {
			continue
		}
		// procRealUID (sidecar_reap_linux.go) is the same real-uid read the
		// boot-time orphan reap uses; an exited-mid-scan process reads ok=false
		// and is treated as "not at this uid".
		if u, ok := procRealUID(pid); ok && u == uid {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitForSidecarGone polls until no tf-sidecar process remains at uid, or fails
// after timeout. Kill→reap is synchronous in inProcessSidecar.Close, but a late
// scheduler reap can lag a beat, so this tolerates a short settle.
func waitForSidecarGone(t *testing.T, uid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(sidecarPidsAtUID(t, uid)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	remaining := sidecarPidsAtUID(t, uid)
	t.Fatalf("EVICTION FAILED: %d tf-sidecar process(es) still live at uid %d after teardown %v — the run's credential address space was not freed", len(remaining), uid, remaining)
}

// runPtraceProbeAsUID execs the probe child (this test binary, via
// sidecarBinaryPath) dropped to uid, asking it to PTRACE_ATTACH targetPid, and
// returns its single PTRACE=... stdout line. Root-gated by the caller (only root
// can setuid to another band).
func runPtraceProbeAsUID(t *testing.T, uid, targetPid int) string {
	t.Helper()
	binPath, err := sidecarBinaryPath()
	if err != nil {
		t.Fatalf("resolve probe binary: %v", err)
	}
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", ptraceProbeEnv, targetPid))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ptrace probe (uid %d → pid %d) failed to run: %v (out: %s)", uid, targetPid, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newAgentHostSocketListener stands up a real host-side agenthost unix socket at
// TrustedAgentHostSocketPath(conversationID) and serves the same one-byte accept the
// cross-tenant probe uses, so a dial that reaches it succeeds. It grants the
// socket to the sandbox identity exactly as the production root path does
// (grantSocketToSandbox: chown WorktreeUID + 0600), so the run's OWN sandbox can
// connect to it through the bind mount. Returns the host path; registers
// cleanup.
func newAgentHostSocketListener(t *testing.T, conversationID string) string {
	t.Helper()
	root := trustedAgentHostSocketRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir agenthost socket root %s: %v", root, err)
	}
	sockPath := TrustedAgentHostSocketPath(conversationID)
	_ = os.Remove(sockPath) // clear any stale file
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen agenthost socket %s: %v", sockPath, err)
	}
	if err := os.Chown(sockPath, WorktreeUID, WorktreeGID); err != nil {
		_ = ln.Close()
		t.Skipf("chown agenthost socket to WorktreeUID (need root): %v", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		t.Fatalf("chmod agenthost socket: %v", err)
	}
	acceptEcho(t, ln)
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})
	return sockPath
}
