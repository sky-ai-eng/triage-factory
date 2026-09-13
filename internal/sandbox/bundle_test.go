//go:build linux

package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestWriteBundle_RoundTrip(t *testing.T) {
	// Fake rootfs to symlink into the bundle.
	rootfs := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootfs, "marker"), []byte("rootfs-here"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := canonicalConfig()
	spec, err := buildSpec(cfg, "/var/run/netns/tf-test")
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	mountCountBefore := len(spec.Mounts)

	bundleDir, err := writeBundle(cfg, spec, rootfs)
	if err != nil {
		t.Fatalf("writeBundle: %v", err)
	}
	t.Cleanup(func() { _ = cleanupBundle(bundleDir) })

	// config.json present and round-trips.
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var got specs.Spec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}

	// rootfs is a symlink to the source.
	target, err := os.Readlink(filepath.Join(bundleDir, "rootfs"))
	if err != nil {
		t.Fatalf("readlink rootfs: %v", err)
	}
	if target != rootfs {
		t.Errorf("rootfs symlink target = %q, want %q", target, rootfs)
	}

	// resolv.conf written.
	if _, err := os.Stat(filepath.Join(bundleDir, "resolv.conf")); err != nil {
		t.Errorf("resolv.conf missing: %v", err)
	}

	// resolv.conf bind mount appended to spec.
	if len(got.Mounts) != mountCountBefore+1 {
		t.Errorf("spec.Mounts count: before=%d, after=%d (want +1 for resolv.conf)",
			mountCountBefore, len(got.Mounts))
	}
	foundResolv := false
	for _, m := range got.Mounts {
		if m.Destination == "/etc/resolv.conf" {
			foundResolv = true
		}
	}
	if !foundResolv {
		t.Errorf("spec.Mounts missing /etc/resolv.conf bind mount")
	}
}

func TestCleanupBundle_Idempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tf-bundle-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cleanupBundle(dir); err != nil {
		t.Errorf("first cleanup: %v", err)
	}
	if err := cleanupBundle(dir); err != nil {
		t.Errorf("second cleanup (idempotent): %v", err)
	}
	if err := cleanupBundle(""); err != nil {
		t.Errorf("empty path cleanup: %v", err)
	}
}
