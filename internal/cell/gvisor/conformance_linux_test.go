//go:build linux && integration

package gvisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/capbroker"
	"github.com/sky-ai-eng/triage-factory/cmd/runsidecar"
	"github.com/sky-ai-eng/triage-factory/internal/cell/celltest"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// The suite runs gVisor cells exactly as an executor does: through a real
// cap-broker, which execs this test binary as `cap-broker`, and which in turn
// execs it as `run-sidecar` at each cell's sidecar uid. TestMain serves both
// before the tests run, the way cli.go serves them in the shipped binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "cap-broker":
			capbroker.Handle(os.Args[2:])
			os.Exit(0)
		case "run-sidecar":
			runsidecar.Handle(os.Args[2:])
			os.Exit(0)
		}
	}
	reexecFromTraversableCopy()
	code := m.Run()
	if dir := os.Getenv(reexecDirEnv); dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

func TestConformance(t *testing.T) {
	requirePrereqs(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	broker, ops, err := capbroker.Start(ctx)
	if err != nil {
		t.Fatalf("start cap-broker: %v", err)
	}
	defer func() { _ = broker.Close() }()
	sandbox.SetPrivilegedOps(ops)

	if _, err := ops.EnsureRootfs(ctx, sandbox.RootfsSelector{}); err != nil {
		t.Skipf("the sandbox rootfs cannot be prepared on this host (%v); the suite needs a built rootfs", err)
	}

	celltest.Run(t, celltest.Harness{Backend: New()})
}

// requirePrereqs skips the suite on a host that cannot build gVisor cells,
// naming what is missing.
func requirePrereqs(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the cap-broker needs root; run the suite as root")
	}
	for _, bin := range []string{"runsc", "ip", "iptables"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	if _, err := os.Stat(sandbox.TrustedToolHostBinaryPath()); err != nil {
		t.Skipf("the tool host binary is not at %s (build harness/tf-harness-tools for the musl target and install it there): %v", sandbox.TrustedToolHostBinaryPath(), err)
	}
}

// reexecDirEnv names the directory a re-exec'd copy of this binary lives in,
// so the copy can remove it when the suite ends.
const reexecDirEnv = "TF_CELLTEST_BIN_DIR"

// reexecFromTraversableCopy re-execs this binary from a world-traversable
// copy when its own directory is not. The broker starts each sidecar from this
// binary at the cell's sidecar uid, which cannot reach a binary inside go
// test's private build directory.
func reexecFromTraversableCopy() {
	if os.Getenv(reexecDirEnv) != "" {
		return
	}
	self, err := os.Executable()
	if err != nil || traversableByAll(filepath.Dir(self)) {
		return
	}
	dir, err := os.MkdirTemp("", "celltest-bin-")
	if err != nil {
		return
	}
	dst := filepath.Join(dir, filepath.Base(self))
	if err := os.Chmod(dir, 0o755); err != nil || copyExecutable(self, dst) != nil {
		_ = os.RemoveAll(dir)
		return
	}
	_ = os.Setenv(reexecDirEnv, dir)
	if err := syscall.Exec(dst, os.Args, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "celltest: re-exec from %s failed: %v\n", dst, err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
}

// traversableByAll reports whether every directory from dir up to / grants
// search permission to everyone.
func traversableByAll(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err != nil || info.Mode().Perm()&0o001 == 0 {
			return false
		}
		if d == filepath.Dir(d) {
			return true
		}
	}
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
