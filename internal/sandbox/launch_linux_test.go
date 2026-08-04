//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// dialStdio stands in for the broker's socket-by-path handoff: it dials the
// orchestrator's per-run stdio socket, giving the connection whose fd
// LaunchSupervised wires to the runtime.
func dialStdio(t *testing.T, sockPath string) *net.UnixConn {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn.(*net.UnixConn)
}

// TestLaunchSupervised_SocketPassthrough proves the core socket-by-path
// property with a bidirectional NDJSON echo (cat) standing in for runsc:
// the runtime reads its stdin and writes its stdout, both wired to the same
// passed-through socket, while LaunchSupervised never reads the fd and
// closes its own copy after Start. The orchestrator's accepted end
// round-trips a line exactly as it would over a runsc-backed stdio.
func TestLaunchSupervised_SocketPassthrough(t *testing.T) {
	orig := buildRuntimeCmd
	buildRuntimeCmd = func(ctx context.Context, _, _ string) *exec.Cmd {
		// cat: read stdin, write stdout — both the same socket fd.
		return exec.CommandContext(ctx, "cat")
	}
	t.Cleanup(func() { buildRuntimeCmd = orig })

	sockPath := filepath.Join(t.TempDir(), "stdio.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Broker side: dial, take the fd, hand it to the runtime, drop copies.
	dialConn := dialStdio(t, sockPath)
	f, err := dialConn.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	sr, err := LaunchSupervised(context.Background(), "", "cid", 0, f, io.Discard)
	_ = dialConn.Close() // broker holds no stdio fd now (f closed by LaunchSupervised)
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}

	// Orchestrator side: accept our end of the passed-through socket.
	orchConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer orchConn.Close()

	line := `{"kind":"user_message","text":"hi"}` + "\n"
	if _, err := io.WriteString(orchConn, line); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(orchConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != line {
		t.Errorf("NDJSON round-trip = %q, want %q", got, line)
	}

	// Half-close our write side → the runtime's stdin EOFs → it exits.
	if err := orchConn.(*net.UnixConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	oom, werr := sr.Wait()
	if werr != nil {
		t.Errorf("Wait: %v", werr)
	}
	if oom {
		t.Error("OOMKilled true with no memory limit configured")
	}
	// No memory limit → no per-run cgroup → nothing to measure. Absent, not
	// zeroes: a run whose cost was never observed must not record one.
	if actuals := sr.Actuals(); actuals.PeakMemMB != nil || actuals.CPUUsec != nil {
		t.Errorf("actuals = %+v with no cgroup, want both absent", actuals)
	}
}

// TestLaunchSupervised_Kill proves the broker can terminate a supervised
// runtime that ignores its stdio (sleep stand-in) via Kill, and that Wait
// then reports the non-nil exit.
func TestLaunchSupervised_Kill(t *testing.T) {
	orig := buildRuntimeCmd
	buildRuntimeCmd = func(ctx context.Context, _, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	}
	t.Cleanup(func() { buildRuntimeCmd = orig })

	sockPath := filepath.Join(t.TempDir(), "stdio.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dialConn := dialStdio(t, sockPath)
	f, err := dialConn.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	sr, err := LaunchSupervised(context.Background(), "", "cid", 0, f, io.Discard)
	_ = dialConn.Close()
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}
	orchConn, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer orchConn.Close()

	if err := sr.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, werr := sr.Wait(); done <- werr }()
	select {
	case werr := <-done:
		if werr == nil {
			t.Error("Wait returned nil after Kill; want a non-nil (signal: killed) exit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
}

// syncBuffer is the broker's stderr sink in these tests. The tap forwards on
// its own goroutine, so the sink is written concurrently with the test's
// reads even though the drain grace normally orders them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// launchStub wires a stand-in runtime and a stand-in `runsc delete`, both
// appending to one ordered event log, and returns the log accessor. It is
// the seam pair that lets the whole state-hygiene sequence be asserted on a
// host with no gVisor.
func launchStub(t *testing.T, mkRuntime func(ctx context.Context) *exec.Cmd) func() []string {
	t.Helper()
	var mu sync.Mutex
	var events []string
	add := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}

	origRun := buildRuntimeCmd
	buildRuntimeCmd = func(ctx context.Context, _, containerID string) *exec.Cmd {
		add("run:" + containerID)
		return mkRuntime(ctx)
	}
	t.Cleanup(func() { buildRuntimeCmd = origRun })

	origDelete := buildRuntimeDeleteCmd
	buildRuntimeDeleteCmd = func(ctx context.Context, containerID string) *exec.Cmd {
		add("delete:" + containerID)
		return exec.CommandContext(ctx, "true")
	}
	t.Cleanup(func() { buildRuntimeDeleteCmd = origDelete })

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}
}

// tmpStdio stands in for the passed-through stdio socket fd for the tests
// that only care about the runtime's stderr. LaunchSupervised takes
// ownership and closes it.
func tmpStdio(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdio")
	if err != nil {
		t.Fatalf("temp stdio: %v", err)
	}
	return f
}

// TestLaunchSupervised_ClearsStaleStateBeforeStarting is the fix for the
// stop→resume collision: container ids recur across engagements of one run
// (same run id, and the allocator hands back the index the stop just freed),
// and a SIGKILLed predecessor leaves state runsc then refuses to create
// over. The launch must clear that state BEFORE it builds the run.
func TestLaunchSupervised_ClearsStaleStateBeforeStarting(t *testing.T) {
	events := launchStub(t, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	})

	sr, err := LaunchSupervised(context.Background(), "", "tf-conv-0", 0, tmpStdio(t), io.Discard)
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}
	if _, err := sr.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := events()
	if len(got) < 2 || got[0] != "delete:tf-conv-0" || got[1] != "run:tf-conv-0" {
		t.Fatalf("launch sequence = %v, want the delete of tf-conv-0 to precede its run", got)
	}
}

// TestSupervisedRuntimeWait_ClearsStateAfterReap pins the eager half: the
// id's state goes away when the jail is reaped, not at some later launch
// that happens to reuse the id. Wait is the one path every death funnels
// through — a natural exit, a Kill, a broker shutdown alike.
func TestSupervisedRuntimeWait_ClearsStateAfterReap(t *testing.T) {
	events := launchStub(t, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	})

	sr, err := LaunchSupervised(context.Background(), "", "tf-conv-0", 0, tmpStdio(t), io.Discard)
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}
	if err := sr.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := sr.Wait(); err == nil {
		t.Error("Wait returned nil after Kill; want a non-nil (signal: killed) exit")
	}

	got := events()
	if len(got) != 3 || got[2] != "delete:tf-conv-0" {
		t.Fatalf("sequence = %v, want a delete of tf-conv-0 after the killed jail was reaped", got)
	}
}

// TestLaunchSupervised_StderrReachesBothSinks is the reporting fix. runsc's
// own create failures go to the broker's stderr by design, which left the
// orchestrator's "tool host exited before connecting" error rendering an
// empty stderr on every brokered launch. The tail must be retained without
// taking the boot log away from the broker's logs.
func TestLaunchSupervised_StderrReachesBothSinks(t *testing.T) {
	const msg = `creating container: container with id "tf-conv-0" already exists`
	launchStub(t, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo '"+msg+"' >&2; exit 128")
	})

	sink := &syncBuffer{}
	sr, err := LaunchSupervised(context.Background(), "", "tf-conv-0", 0, tmpStdio(t), sink)
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}
	if _, err := sr.Wait(); err == nil {
		t.Error("Wait returned nil for a runtime that exited 128")
	}

	if !strings.Contains(sr.StderrTail(), "already exists") {
		t.Errorf("StderrTail = %q, want runsc's own failure; a dead jail must be able to say why", sr.StderrTail())
	}
	if !strings.Contains(sink.String(), "already exists") {
		t.Errorf("broker stderr sink = %q, want the boot log still forwarded to it", sink.String())
	}
}

// TestLaunchSupervised_StderrTailIsBounded proves the tail is a tail: a
// chatty jail contributes its last few KB to an error message, never a dump.
func TestLaunchSupervised_StderrTailIsBounded(t *testing.T) {
	launchStub(t, func(ctx context.Context) *exec.Cmd {
		// ~64 KiB of stderr, ending in a marker the tail must retain.
		return exec.CommandContext(ctx, "sh", "-c",
			`i=0; while [ $i -lt 2048 ]; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n' >&2; i=$((i+1)); done; echo LAST_LINE >&2`)
	})

	sr, err := LaunchSupervised(context.Background(), "", "tf-conv-0", 0, tmpStdio(t), io.Discard)
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}
	if _, err := sr.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	tail := sr.StderrTail()
	if !strings.Contains(tail, "LAST_LINE") {
		t.Error("tail dropped the last thing the jail said, which is the part that explains the exit")
	}
	// The truncation marker adds a few bytes on top of the ring itself.
	if len(tail) > stderrTailBytes+64 {
		t.Errorf("tail is %d bytes, want at most %d — it is not bounded", len(tail), stderrTailBytes)
	}
}

// TestLaunchSupervised_StderrTailEmptyBeforeWait pins the same contract
// Actuals has: the tail is captured on the exit path, so reading it earlier
// reports nothing rather than a partial line.
func TestLaunchSupervised_StderrTailEmptyBeforeWait(t *testing.T) {
	launchStub(t, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "60")
	})

	sr, err := LaunchSupervised(context.Background(), "", "tf-conv-0", 0, tmpStdio(t), io.Discard)
	if err != nil {
		t.Fatalf("LaunchSupervised: %v", err)
	}
	if got := sr.StderrTail(); got != "" {
		t.Errorf("StderrTail before Wait = %q, want empty", got)
	}
	_ = sr.Kill()
	_, _ = sr.Wait()
}
