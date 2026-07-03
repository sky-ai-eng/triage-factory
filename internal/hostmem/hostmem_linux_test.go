//go:build linux

package hostmem

import "testing"

// On any real Linux host both figures are readable and positive, and
// available never exceeds total. This pins the /proc parsing without
// fixture files — the fields have been stable since kernel 3.14.
func TestMeminfoFigures(t *testing.T) {
	avail := AvailableMB()
	total := TotalMB()
	if avail <= 0 {
		t.Fatalf("AvailableMB() = %d, want > 0 on Linux", avail)
	}
	if total <= 0 {
		t.Fatalf("TotalMB() = %d, want > 0 on Linux", total)
	}
	if avail > total {
		t.Errorf("AvailableMB() %d > TotalMB() %d", avail, total)
	}
}
