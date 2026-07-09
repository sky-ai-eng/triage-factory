//go:build linux

package capbroker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
)

// helperProcessEnv gates TestMain's re-exec branch below — the standard
// os/exec "TestHelperProcess" pattern (see the os/exec package's own
// tests): a test overrides execSelfCommand to re-invoke this same test
// binary with this env var set, and TestMain runs the real runBroker
// instead of the test suite, so Start/Process.Close are exercised against
// an actual subprocess without needing the full triagefactory binary.
const helperProcessEnv = "CAPBROKER_TEST_HELPER_PROCESS"

// capReportEnv gates a second TestMain re-exec branch (privsep_capdrop_linux_test.go):
// instead of running the broker, the re-invoked test binary reports its own
// uid and effective capability set and exits — the thing a real setpriv
// invocation is applied *around*, so the test can observe the actual kernel
// state a real exec-time capability drop produces.
const capReportEnv = "CAPBROKER_TEST_CAPREPORT"

func TestMain(m *testing.M) {
	if os.Getenv(helperProcessEnv) == "1" {
		args := os.Args[1:]
		for i, a := range args {
			if a == "--" {
				args = args[i+1:]
				break
			}
		}
		if err := runBroker(args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(capReportEnv) == "1" {
		names, err := capinfo.Effective()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("uid=%d CapEff=%s\n", os.Getuid(), capinfo.Describe(names))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// withHelperExecSelfCommand points execSelfCommand at a re-exec of this
// test binary (see TestMain) instead of os.Executable(), and restores the
// original on cleanup.
func withHelperExecSelfCommand(t *testing.T) {
	t.Helper()
	self := os.Args[0]
	orig := execSelfCommand
	execSelfCommand = func(socketPath string) (*exec.Cmd, error) {
		cmd := exec.Command(self, "--", "--socket", socketPath)
		cmd.Env = append(os.Environ(), helperProcessEnv+"=1")
		return cmd, nil
	}
	t.Cleanup(func() { execSelfCommand = orig })
}

// withTempBrokerSocketPath redirects Start's socket path to an isolated
// temp directory instead of the production /run/tf/cap-broker.sock —
// root-only on most distros, and unwritable to the unprivileged user a CI
// `go test` runs as. listen()'s MkdirAll(filepath.Dir(socketPath)) makes
// this path fully self-contained: nothing under /run is touched.
func withTempBrokerSocketPath(t *testing.T) string {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "cap-broker.sock")
	orig := brokerSocketPath
	brokerSocketPath = sockPath
	t.Cleanup(func() { brokerSocketPath = orig })
	return sockPath
}

// TestStart_SpawnsAndBecomesReady exercises the real Start/Process.Close
// lifecycle against a subprocess (the re-exec'd test binary running the
// actual runBroker), the same mechanism production uses — just with a
// stand-in for the real `<bin> cap-broker` re-exec and an isolated socket
// path so this doesn't need root.
func TestStart_SpawnsAndBecomesReady(t *testing.T) {
	sockPath := withTempBrokerSocketPath(t)
	withHelperExecSelfCommand(t)

	proc, client, err := Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer proc.Close()

	// The broker is live and serving: a real RPC round-trips.
	if err := client.(*IPCClient).Ping(context.Background()); err != nil {
		t.Errorf("Ping against started broker: %v", err)
	}

	// A real privileged op — ReapOrphans, idempotent and side-effect-free on
	// a clean host — round-trips through the actual sandbox.NewHostOps()
	// running inside the spawned subprocess, not a fake. This is the
	// acceptance bullet's "network/cgroup/rootfs/teardown ops execute in
	// the cap-broker process" proven at the mechanism level: a real RPC
	// reaching real hostOps in a real separate process.
	if err := client.ReapOrphans(context.Background()); err != nil {
		t.Errorf("ReapOrphans through the started broker: %v", err)
	}

	if err := proc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Removed on graceful shutdown (cli_linux.go's runBroker defer).
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file should be removed after graceful shutdown, stat err = %v", err)
	}
}

// TestStart_DialsExistingBrokerInsteadOfSpawning pins the default
// container-deployment path: when docker/entrypoint.sh has already
// spawned a broker before exec'ing this (capability-dropped) process
// into existence, Start must dial it rather than trying (and,
// post-drop, failing) to spawn a second one — and the returned Process
// must not try to kill a broker it never started.
func TestStart_DialsExistingBrokerInsteadOfSpawning(t *testing.T) {
	sockPath := withTempBrokerSocketPath(t)
	withHelperExecSelfCommand(t)

	// Simulate the container entrypoint: spawn a broker directly,
	// bypassing Start entirely, standing in for "some other ancestor
	// process already started it."
	preexisting, err := execSelfCommand(sockPath)
	if err != nil {
		t.Fatalf("spawn pre-existing broker: %v", err)
	}
	if err := preexisting.Start(); err != nil {
		t.Fatalf("start pre-existing broker: %v", err)
	}
	defer func() {
		_ = preexisting.Process.Kill()
		_ = preexisting.Wait()
	}()
	if err := waitReady(context.Background(), Dial(sockPath)); err != nil {
		t.Fatalf("pre-existing broker never became ready: %v", err)
	}

	// Make a second spawn attempt fail the test loudly — proving Start
	// took the dial-first path instead of falling through to spawn.
	execSelfCommand = func(socketPath string) (*exec.Cmd, error) {
		t.Fatal("Start spawned a second broker instead of dialing the existing one")
		return nil, nil
	}

	proc, client, err := Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if proc.cmd != nil {
		t.Error("Process.cmd = non-nil, want nil: a dial-only Process never spawned the broker and must not try to manage its lifecycle")
	}
	if err := client.(*IPCClient).Ping(context.Background()); err != nil {
		t.Errorf("Ping against the dialed broker: %v", err)
	}

	// Close must be a safe no-op here: it must NOT kill the pre-existing
	// broker, which this Process doesn't own.
	if err := proc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := Dial(sockPath).Ping(context.Background()); err != nil {
		t.Errorf("pre-existing broker should still be reachable after proc.Close(): %v", err)
	}
}

// TestStart_BrokerNeverComesUp pins the "broker crash mid-run surfaces a
// legible error and does not wedge the orchestrator" acceptance bullet:
// Start against a helper that exits immediately (never listens) fails
// fast with a clear error rather than hanging for readyTimeout's full
// duration or forever.
func TestStart_BrokerNeverComesUp(t *testing.T) {
	withTempBrokerSocketPath(t)
	origCmd := execSelfCommand
	origTimeout, origPoll := readyTimeout, readyPollInterval
	readyTimeout = 300 * time.Millisecond
	readyPollInterval = 10 * time.Millisecond
	execSelfCommand = func(socketPath string) (*exec.Cmd, error) {
		// A command that exits immediately without ever listening —
		// stands in for a broker that crashes on startup.
		return exec.Command("true"), nil
	}
	t.Cleanup(func() {
		execSelfCommand = origCmd
		readyTimeout, readyPollInterval = origTimeout, origPoll
	})

	start := time.Now()
	_, _, err := Start(context.Background())
	if err == nil {
		t.Fatal("expected an error when the broker subprocess never comes up")
	}
	if elapsed := time.Since(start); elapsed > readyTimeout+2*time.Second {
		t.Errorf("Start took %s to fail, want close to readyTimeout (%s)", elapsed, readyTimeout)
	}
}

// TestStart_ContextCanceledDuringWait pins that waitReady honors ctx
// cancellation instead of always riding out the full readyTimeout: a
// caller giving up early (e.g. its own request context expiring) must not
// be stuck waiting behind a broker that's slow to come up.
func TestStart_ContextCanceledDuringWait(t *testing.T) {
	withTempBrokerSocketPath(t)
	origCmd := execSelfCommand
	origTimeout := readyTimeout
	readyTimeout = 5 * time.Second // long enough that only cancellation, not the timeout, could explain a fast return
	execSelfCommand = func(socketPath string) (*exec.Cmd, error) {
		// Never creates the socket — stands in for a broker that's slow
		// (not crashed) to come up, so only ctx cancellation ends the wait.
		return exec.Command("sleep", "30"), nil
	}
	t.Cleanup(func() {
		execSelfCommand = origCmd
		readyTimeout = origTimeout
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before Start begins waiting

	start := time.Now()
	_, _, err := Start(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error for an already-canceled context")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Start took %s with an already-canceled context, want a near-immediate return (readyTimeout is %s)", elapsed, readyTimeout)
	}
}

// TestProcess_CloseIsIdempotentAndNilSafe pins that Close never panics on
// a nil Process or a double call — the orchestrator's App.Close() calls
// this unconditionally.
func TestProcess_CloseIsIdempotentAndNilSafe(t *testing.T) {
	var nilProc *Process
	if err := nilProc.Close(); err != nil {
		t.Errorf("nil Process Close() = %v, want nil", err)
	}

	withTempBrokerSocketPath(t)
	withHelperExecSelfCommand(t)
	proc, _, err := Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := proc.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := proc.Close(); err != nil {
		t.Errorf("second Close (idempotent) = %v, want nil", err)
	}
}
