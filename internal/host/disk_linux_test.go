//go:build linux

package host

import (
	"path/filepath"
	"testing"
)

func TestDiskAvailableMB(t *testing.T) {
	dir := t.TempDir()
	if got := DiskAvailableMB(dir); got == Unknown || got < 0 {
		t.Fatalf("DiskAvailableMB(%s) = %d, want a figure", dir, got)
	}
	// A path not created yet is skipped, not a failure of the whole read.
	missing := filepath.Join(dir, "not-yet")
	if got, want := DiskAvailableMB(missing, dir), DiskAvailableMB(dir); got != want {
		t.Errorf("DiskAvailableMB(missing, dir) = %d, want %d (the missing path skipped)", got, want)
	}
	if got := DiskAvailableMB(missing); got != Unknown {
		t.Errorf("DiskAvailableMB with no readable path = %d, want Unknown", got)
	}
	if got := DiskAvailableMB(); got != Unknown {
		t.Errorf("DiskAvailableMB() = %d, want Unknown", got)
	}
}
