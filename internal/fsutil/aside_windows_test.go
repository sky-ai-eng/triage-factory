//go:build windows

package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsAsideName(t *testing.T) {
	for name, want := range map[string]bool{
		"triagefactory.exe.old":     true,
		"TriageFactory.EXE.OLD":     true,
		"triagefactory.exe.old.1":   true,
		"triagefactory.exe.old.42":  true,
		"triagefactory.exe":         false,
		"triagefactory.exe.old.":    false,
		"triagefactory.exe.old.bak": false,
		"triagefactory.exe.old.1a":  false,
		"triagefactory.exe.old.1.2": false,
		"triagefactory.exe.oldnote": false,
		"other.exe.old":             false,
	} {
		if got := isAsideName("triagefactory.exe", name); got != want {
			t.Errorf("isAsideName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRemoveAsides_RemovesOnlyAsides(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "triagefactory.exe")
	for name, content := range map[string]string{
		"triagefactory.exe.old":     "a",
		"triagefactory.exe.old.3":   "b",
		"triagefactory.exe.old.bak": "keep",
		"triagefactory.exe.oldnote": "keep",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if remaining := RemoveAsides(target); len(remaining) != 0 {
		t.Fatalf("remaining = %v, want none", remaining)
	}
	for _, name := range []string{"triagefactory.exe.old", "triagefactory.exe.old.3"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat err = %v", name, err)
		}
	}
	for _, name := range []string{"triagefactory.exe.old.bak", "triagefactory.exe.oldnote"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is not an aside and must survive: %v", name, err)
		}
	}
}

func TestAsidePath_SkipsAHeldAside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "triagefactory.exe")
	held := target + ".old"
	if err := os.WriteFile(held, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	// os.Open shares read/write but not delete, so the file cannot be removed
	// while held, as with a running image.
	f, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if remaining := RemoveAsides(target); len(remaining) != 1 || remaining[0] != held {
		t.Errorf("remaining = %v, want [%s]", remaining, held)
	}
	got, err := AsidePath(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := target + ".old.1"; got != want {
		t.Errorf("AsidePath = %q, want %q", got, want)
	}
}

func TestAsidePath_PrefersTheUnnumberedName(t *testing.T) {
	target := filepath.Join(t.TempDir(), "triagefactory.exe")
	got, err := AsidePath(target)
	if err != nil || got != target+".old" {
		t.Errorf("AsidePath = %q, %v; want %s.old", got, err, target)
	}
}
