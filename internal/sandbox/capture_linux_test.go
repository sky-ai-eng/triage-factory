//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// captureTestTree returns a directory that passes validateRunTreeRoot:
// t.TempDir() is absolute, clean, ≥2 components deep, symlink-free on
// Linux, and owned by the test process's own uid.
func captureTestTree(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestCaptureRunDelta_DropsPrivilegeAndNetns pins the containment the
// capture relies on: the capture child runs as the sandbox uid/gid with the
// parent's supplementary groups shed, inside an empty network namespace.
// That is what confines a filter a hostile .git/config could trigger to the
// agent's own privilege instead of the privileged capture host (the
// cap-broker — the escape this capture path closes).
//
// It overrides captureCommand with a shell that reports `id` and
// /proc/net/dev to a side file (stdout stays clean — it echoes the "null"
// delta), then asserts on that file. Needs root to setuid/setgroups/unshare,
// so it skips otherwise — the same gate as the sandbox integration tests.
func TestCaptureRunDelta_DropsPrivilegeAndNetns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to setuid/setgroups/unshare a network namespace")
	}

	diag, err := os.CreateTemp("", "tf-capdrop-*")
	if err != nil {
		t.Fatal(err)
	}
	diagPath := diag.Name()
	_ = diag.Close()
	// The child runs as uid 10000; it must be able to write the diag file.
	if err := os.Chmod(diagPath, 0o666); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(diagPath) })

	orig := captureCommand
	captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
		script := "{ id; echo ==NET==; cat /proc/net/dev; } > " + diagPath + " 2>&1; echo null"
		return exec.CommandContext(ctx, "/bin/sh", "-c", script), nil
	}
	t.Cleanup(func() { captureCommand = orig })

	raw, err := (hostOps{}).CaptureRunDelta(context.Background(), captureTestTree(t))
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "null" {
		t.Errorf("raw output = %q, want the child's literal %q", got, "null")
	}

	b, err := os.ReadFile(diagPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	idLine, netPart := out, ""
	if i := strings.Index(out, "==NET=="); i >= 0 {
		idLine, netPart = out[:i], out[i:]
	}

	if !strings.Contains(idLine, "uid=10000") || !strings.Contains(idLine, "gid=10000") {
		t.Errorf("child did not drop to uid/gid 10000:\n%s", idLine)
	}
	if strings.Contains(idLine, "(root)") || strings.Contains(idLine, "groups=0") {
		t.Errorf("child retained a root supplementary group:\n%s", idLine)
	}

	// A fresh CLONE_NEWNET namespace contains ONLY loopback. Assert exactly that
	// — the positive property — rather than blocklisting known host interface
	// names, so an interface with any name (enpXsY, wlan0, …) leaking in fails.
	ifaces := parseProcNetDevIfaces(netPart)
	if len(ifaces) == 0 {
		t.Fatalf("could not parse any interface from /proc/net/dev:\n%s", netPart)
	}
	for _, name := range ifaces {
		if name != "lo" {
			t.Errorf("child network namespace not isolated (interface %q present, want only lo): %v", name, ifaces)
		}
	}
}

// TestCaptureRunDelta_SkipsNetnsWithoutSysAdmin pins the fallback the
// unprivileged bare-metal dev case relies on: creating a network namespace
// needs CAP_SYS_ADMIN, so the capture must skip CLONE_NEWNET rather than
// fail the whole capture on a privileged syscall it can never make. The
// uid/gid drop — the primary boundary — still applies regardless. (In the
// default deployment this method runs in the cap-broker, which always
// holds CAP_SYS_ADMIN, so the netns applies unconditionally there.)
//
// Asserts via /proc/self/ns/net identity (a symlink to "net:[<inode>]",
// unique per network namespace on the host) rather than comparing
// interface *counts*: a fresh netns also starts with only "lo", so on a
// host that itself has only "lo" (a minimal/isolated CI runner), a
// count-based comparison would pass whether or not CLONE_NEWNET was
// actually (wrongly) still applied. Namespace identity has no such
// false-pass — it directly answers "is this the same network namespace
// as the parent," independent of the host's own interface topology.
func TestCaptureRunDelta_SkipsNetnsWithoutSysAdmin(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to setuid/setgroups the child")
	}

	wantNetns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatalf("readlink /proc/self/ns/net: %v", err)
	}

	origHasSysAdmin := hasSysAdmin
	hasSysAdmin = func() bool { return false }
	t.Cleanup(func() { hasSysAdmin = origHasSysAdmin })

	diag, err := os.CreateTemp("", "tf-capdrop-nonetns-*")
	if err != nil {
		t.Fatal(err)
	}
	diagPath := diag.Name()
	_ = diag.Close()
	if err := os.Chmod(diagPath, 0o666); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(diagPath) })

	orig := captureCommand
	captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
		script := "{ id; echo ==NETNS==; readlink /proc/self/ns/net; } > " + diagPath + " 2>&1; echo null"
		return exec.CommandContext(ctx, "/bin/sh", "-c", script), nil
	}
	t.Cleanup(func() { captureCommand = orig })

	if _, err := (hostOps{}).CaptureRunDelta(context.Background(), captureTestTree(t)); err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}

	b, err := os.ReadFile(diagPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	idLine, gotNetns := out, ""
	if i := strings.Index(out, "==NETNS=="); i >= 0 {
		idLine, gotNetns = out[:i], strings.TrimSpace(out[i+len("==NETNS=="):])
	}

	if !strings.Contains(idLine, "uid=10000") || !strings.Contains(idLine, "gid=10000") {
		t.Errorf("child did not drop to uid/gid 10000 even without CAP_SYS_ADMIN:\n%s", idLine)
	}

	if gotNetns != wantNetns {
		t.Errorf("child network namespace = %q, want %q (the parent's own — hasSysAdmin() = false should mean no new netns was created)", gotNetns, wantNetns)
	}
}

// TestCaptureRunDeltaTo_StdoutIsTheSocketBackedFile pins the one
// implementation trap this ticket exists to avoid: the child's stdout must
// be the caller-supplied *os.File itself, assigned directly to cmd.Stdout —
// never wrapped in another io.Writer. os/exec special-cases *os.File (a
// direct dup2 into the child at exec time, no copy in this process);
// anything else makes os/exec silently fall back to an os.Pipe() plus a
// copy goroutine IN THIS PROCESS, reintroducing every byte of the delta
// into the caller's (in production, the broker's) memory and defeating the
// whole point of CaptureRunDeltaTo.
//
// Uses a nonexistent binary so cmd.Start() fails fast (no such file) without
// ever needing to setuid — the point here is only what cmd.Stdout was set
// to before Start was attempted, not whether the child actually ran.
func TestCaptureRunDeltaTo_StdoutIsTheSocketBackedFile(t *testing.T) {
	orig := captureCommand
	var gotCmd *exec.Cmd
	captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
		gotCmd = exec.CommandContext(ctx, "/nonexistent-tf-capture-pin-test-binary")
		return gotCmd, nil
	}
	t.Cleanup(func() { captureCommand = orig })

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pr.Close() })

	// The child can never actually start (no such binary) — irrelevant here;
	// CaptureRunDeltaTo still closes pw regardless of outcome.
	_, _ = CaptureRunDeltaTo(context.Background(), captureTestTree(t), pw)

	if gotCmd == nil {
		t.Fatal("captureCommand was never invoked")
	}
	if gotCmd.Stdout != pw {
		t.Errorf("cmd.Stdout = %#v, want the exact *os.File %v passed to CaptureRunDeltaTo", gotCmd.Stdout, pw)
	}
}

// TestReadCapturedDelta_UnderCapByteIdentical pins the happy path of the
// shared cap both capture paths enforce: data at or under CaptureMaxBytes
// round-trips byte-identical.
func TestReadCapturedDelta_UnderCapByteIdentical(t *testing.T) {
	origCap := CaptureMaxBytes
	CaptureMaxBytes = 1024
	t.Cleanup(func() { CaptureMaxBytes = origCap })

	want := bytes.Repeat([]byte{0xAB}, 1024)
	got, err := ReadCapturedDelta(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("ReadCapturedDelta: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %d bytes, want %d identical bytes", len(got), len(want))
	}
}

// TestReadCapturedDelta_OverCapErrors pins the loss-contract half of the cap:
// data exceeding CaptureMaxBytes by even one byte is rejected outright
// rather than silently truncated — a truncated-but-otherwise-valid-looking
// bundle would be worse than no delta at all (the park degrades to
// snapshot-less either way, but a truncated one could look intact).
func TestReadCapturedDelta_OverCapErrors(t *testing.T) {
	origCap := CaptureMaxBytes
	CaptureMaxBytes = 1024
	t.Cleanup(func() { CaptureMaxBytes = origCap })

	over := bytes.Repeat([]byte{0xCD}, 1025)
	if _, err := ReadCapturedDelta(bytes.NewReader(over)); err == nil {
		t.Error("expected an over-cap error, got nil")
	}
}

// TestCaptureRunDelta_RejectsInvalidWorktree pins the boundary validation:
// the capture exec must refuse paths a compromised orchestrator could use
// to point the privileged child (or its git invocation) somewhere
// surprising — never spawning the child at all. captureCommand fails the
// test loudly if reached.
func TestCaptureRunDelta_RejectsInvalidWorktree(t *testing.T) {
	orig := captureCommand
	captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
		t.Errorf("captureCommand invoked for invalid worktree %q", wtPath)
		return exec.Command("false"), nil
	}
	t.Cleanup(func() { captureCommand = orig })

	for _, path := range []string{
		"",                               // empty
		"relative/path",                  // not absolute
		"/",                              // the root itself
		"/tmp",                           // one component — never a run tree
		"/tmp/../etc/cron.d",             // non-clean
		"/nonexistent-tf-capture-test/x", // no such tree
	} {
		if _, err := (hostOps{}).CaptureRunDelta(context.Background(), path); err == nil {
			t.Errorf("CaptureRunDelta(%q) = nil error, want validation rejection", path)
		}
	}
}

// parseProcNetDevIfaces extracts interface names from a /proc/net/dev dump.
// Data lines are "  <name>: <counters...>"; the two header lines have no
// pre-colon interface token, so keying on the colon-delimited first field and
// dropping empties yields exactly the interface set.
func parseProcNetDevIfaces(procNetDev string) []string {
	var names []string
	for _, line := range strings.Split(procNetDev, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if name := strings.TrimSpace(line[:i]); name != "" && !strings.Contains(name, "|") {
			names = append(names, name)
		}
	}
	return names
}
