//go:build windows

package uninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installedCopy(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Programs", "triagefactory", "triagefactory.exe")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func otherExe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "triagefactory.exe")
	if err := os.WriteFile(p, []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultInstallLink_UsesLocalAppData(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	want := filepath.Join(local, "Programs", "triagefactory", "triagefactory.exe")
	if got := defaultInstallLink(); got != want {
		t.Errorf("defaultInstallLink = %q, want %q", got, want)
	}
}

func TestRemoveInstalledCopy_RemovesCopyAndEmptyDir(t *testing.T) {
	p := installedCopy(t)
	if err := os.WriteFile(p+".old", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg, err := removeInstalledCopy(p, otherExe(t))
	if err != nil || !strings.Contains(msg, "removed") {
		t.Fatalf("removeInstalledCopy = %q, %v", msg, err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
		t.Errorf("empty install dir should be removed too, stat err = %v", err)
	}
}

func TestRemoveInstalledCopy_KeepsDirWithOtherFiles(t *testing.T) {
	p := installedCopy(t)
	extra := filepath.Join(filepath.Dir(p), "user-notes.txt")
	if err := os.WriteFile(extra, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := removeInstalledCopy(p, otherExe(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Errorf("an unrelated file in the install dir was removed: %v", err)
	}
}

func TestRemoveInstalledCopy_RunningExeIsRenamedAside(t *testing.T) {
	p := installedCopy(t)
	msg, err := removeInstalledCopy(p, p) // the install IS the running executable
	if err != nil {
		t.Fatalf("removeInstalledCopy: %v", err)
	}
	if !strings.Contains(msg, ".old") || !strings.Contains(msg, "delete it after") {
		t.Errorf("message should name the .old file and say to delete it: %q", msg)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("install path should be vacated, stat err = %v", err)
	}
	if b, err := os.ReadFile(p + ".old"); err != nil || string(b) != "installed" {
		t.Errorf(".old = %q, %v; want the original content", b, err)
	}
}

func TestRemoveInstalledCopy_SkipsOtherNames(t *testing.T) {
	p := filepath.Join(t.TempDir(), "something-else.exe")
	if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	msg, err := removeInstalledCopy(p, otherExe(t))
	if err != nil || !strings.Contains(msg, "skipped") {
		t.Fatalf("removeInstalledCopy = %q, %v; want a skip", msg, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("a file we did not install was removed: %v", err)
	}
}

func TestRemoveInstalledCopy_SkipsDirectories(t *testing.T) {
	p := filepath.Join(t.TempDir(), "triagefactory.exe")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	msg, err := removeInstalledCopy(p, otherExe(t))
	if err != nil || !strings.Contains(msg, "skipped") {
		t.Fatalf("removeInstalledCopy = %q, %v; want a skip", msg, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("directory removed: %v", err)
	}
}

func TestRemoveInstalledCopy_MissingIsAnError(t *testing.T) {
	if _, err := removeInstalledCopy(filepath.Join(t.TempDir(), "triagefactory.exe"), otherExe(t)); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
