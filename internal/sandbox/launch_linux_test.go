//go:build linux

package sandbox

import (
	"bufio"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
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
	sr, err := LaunchSupervised(context.Background(), LaunchParams{ContainerID: "cid"}, f, io.Discard)
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
	sr, err := LaunchSupervised(context.Background(), LaunchParams{ContainerID: "cid"}, f, io.Discard)
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
