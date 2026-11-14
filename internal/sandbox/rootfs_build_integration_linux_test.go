//go:build linux && integration

package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireRootfsBuild skips unless this process can build the cached rootfs:
// the build chroots into the extracted minirootfs to run apk, and creates a
// device node in it, both of which need root.
func requireRootfsBuild(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the rootfs build needs root for chroot and mknod; skipping")
	}
	if _, err := exec.LookPath("chroot"); err != nil {
		t.Skip("chroot not installed; skipping rootfs build test")
	}
}

// TestIntegration_RootfsBuilds builds the rootfs under whatever toolchain root
// the environment gives it and runs a package from the toolchain in it. With
// an empty toolchain root the build is cold, so every package's install
// scripts run, which is what this test exists to catch: an apk package whose
// script fails fails the whole build, and with it every sandbox launch.
func TestIntegration_RootfsBuilds(t *testing.T) {
	requireRootfsBuild(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	dir, err := ensureRootfs(ctx)
	if err != nil {
		t.Fatalf("ensureRootfs: %v", err)
	}
	// The cache is shared by every jail on the host and must hold no device
	// node the build made for itself.
	if _, err := os.Lstat(filepath.Join(dir, "dev", "null")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the built rootfs still holds dev/null (lstat: %v)", err)
	}

	out, err := exec.CommandContext(ctx, "chroot", dir, "/usr/bin/git-lfs", "version").CombinedOutput()
	if err != nil {
		t.Fatalf("chroot %s git-lfs version: %v (output: %s)", dir, err, out)
	}
	if !strings.Contains(string(out), "git-lfs/") {
		t.Errorf("git-lfs version output missing %q:\n%s", "git-lfs/", out)
	}
}
