//go:build linux

package hostdisk

import (
	"path/filepath"
	"testing"
)

func TestAvailableMB(t *testing.T) {
	dir := t.TempDir()
	if got := AvailableMB(dir); got == Unknown || got < 0 {
		t.Fatalf("AvailableMB(%s) = %d, want a figure", dir, got)
	}
	// A path not created yet is skipped, not a failure of the whole read.
	missing := filepath.Join(dir, "not-yet")
	if got, want := AvailableMB(missing, dir), AvailableMB(dir); got != want {
		t.Errorf("AvailableMB(missing, dir) = %d, want %d (the missing path skipped)", got, want)
	}
	if got := AvailableMB(missing); got != Unknown {
		t.Errorf("AvailableMB with no readable path = %d, want Unknown", got)
	}
	if got := AvailableMB(); got != Unknown {
		t.Errorf("AvailableMB() = %d, want Unknown", got)
	}
}
