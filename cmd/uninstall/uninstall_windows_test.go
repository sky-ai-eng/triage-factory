//go:build windows

package uninstall

import (
	"errors"
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

// stubScheduleDelete replaces the restart-time delete request for t and returns
// the paths it was asked to delete.
func stubScheduleDelete(t *testing.T, result error) *[]string {
	t.Helper()
	var asked []string
	prev := scheduleDelete
	scheduleDelete = func(path string) error {
		asked = append(asked, path)
		return result
	}
	t.Cleanup(func() { scheduleDelete = prev })
	return &asked
}

func TestRemoveInstalledCopy_RunningExeIsRenamedAndScheduledForDeletion(t *testing.T) {
	asked := stubScheduleDelete(t, nil)
	p := installedCopy(t)
	msg, err := removeInstalledCopy(p, p) // the install IS the running executable
	if err != nil {
		t.Fatalf("removeInstalledCopy: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != p+".old" {
		t.Errorf("scheduled deletions = %v, want [%s.old]", *asked, p)
	}
	if !strings.Contains(msg, ".old") || !strings.Contains(msg, "at the next restart") {
		t.Errorf("message should name the .old file and say its deletion is scheduled: %q", msg)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("install path should be vacated, stat err = %v", err)
	}
	if b, err := os.ReadFile(p + ".old"); err != nil || string(b) != "installed" {
		t.Errorf(".old = %q, %v; want the original content", b, err)
	}
}

func TestRemoveInstalledCopy_RunningExeWhenSchedulingFailsAsksForManualDelete(t *testing.T) {
	stubScheduleDelete(t, errors.New("access denied"))
	p := installedCopy(t)
	msg, err := removeInstalledCopy(p, p)
	if err != nil {
		t.Fatalf("a failed schedule must not fail the uninstall: %v", err)
	}
	if !strings.Contains(msg, "could not schedule") || !strings.Contains(msg, "delete it after this command exits") || !strings.Contains(msg, ".old") {
		t.Errorf("message should say scheduling failed and ask for a manual delete: %q", msg)
	}
	if strings.Contains(msg, "at the next restart") {
		t.Errorf("message claims a scheduled deletion that did not happen: %q", msg)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("install path should be vacated, stat err = %v", err)
	}
	if b, err := os.ReadFile(p + ".old"); err != nil || string(b) != "installed" {
		t.Errorf(".old = %q, %v; want the original content", b, err)
	}
}

func TestRemoveInstalledCopy_RunningExeIsRenamedAside(t *testing.T) {
	stubScheduleDelete(t, nil)
	p := installedCopy(t)
	msg, err := removeInstalledCopy(p, p) // the install IS the running executable
	if err != nil {
		t.Fatalf("removeInstalledCopy: %v", err)
	}
	if !strings.Contains(msg, ".old") {
		t.Errorf("message should name the .old file: %q", msg)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("install path should be vacated, stat err = %v", err)
	}
	if b, err := os.ReadFile(p + ".old"); err != nil || string(b) != "installed" {
		t.Errorf(".old = %q, %v; want the original content", b, err)
	}
}

// An earlier .old that is still locked by a running process takes the next
// numbered name for the new aside, and that is the one scheduled for deletion.
func TestRemoveInstalledCopy_LockedEarlierAsideTakesANumberedName(t *testing.T) {
	asked := stubScheduleDelete(t, nil)
	p := installedCopy(t)
	if err := os.WriteFile(p+".old", []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	// os.Open shares read/write but not delete, so the file cannot be removed
	// while held, as with a running image.
	held, err := os.Open(p + ".old")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	msg, err := removeInstalledCopy(p, p)
	if err != nil {
		t.Fatalf("removeInstalledCopy: %v", err)
	}
	want := p + ".old.1"
	if len(*asked) != 1 || (*asked)[0] != want {
		t.Errorf("scheduled deletions = %v, want [%s]", *asked, want)
	}
	if !strings.Contains(msg, want) || !strings.Contains(msg, "is still in use") {
		t.Errorf("message should name the numbered aside and the locked one: %q", msg)
	}
	if b, err := os.ReadFile(want); err != nil || string(b) != "installed" {
		t.Errorf("%s = %q, %v; want the original content", want, b, err)
	}
	if b, err := os.ReadFile(p + ".old"); err != nil || string(b) != "running" {
		t.Errorf("the locked aside was disturbed: %q, %v", b, err)
	}
}

// Leftover numbered asides are removed with the install, and only those: a
// file that merely starts the same way is not ours.
func TestRemoveInstalledCopy_RemovesLeftoverNumberedAsides(t *testing.T) {
	p := installedCopy(t)
	for _, n := range []string{".old", ".old.1", ".old.12"} {
		if err := os.WriteFile(p+n, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keep := p + ".old.bak"
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := removeInstalledCopy(p, otherExe(t)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", ".old", ".old.1", ".old.12"} {
		if _, err := os.Stat(p + n); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat err = %v", p+n, err)
		}
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" {
		t.Errorf("%s = %q, %v; want it kept", keep, b, err)
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
