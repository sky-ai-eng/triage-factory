//go:build windows

package uninstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

const (
	installArtifact = "installed copy"

	binaryStaysNote = "A triagefactory.exe you run from elsewhere stays — delete it by hand."
	binaryDoneNote  = "To remove a triagefactory.exe you ran this from, delete it by hand."

	// installedName is the only file name uninstall will remove: the one
	// `triagefactory install` writes.
	installedName = "triagefactory.exe"
)

// defaultInstallLink is where `triagefactory install` puts its copy on Windows.
// A copy installed with --dest elsewhere is not discovered, for the same reason
// as on Unix: a false positive would delete a file we did not install.
func defaultInstallLink() string {
	p, err := paths.WindowsInstallPath()
	if err != nil {
		return ""
	}
	return p
}

// removeInstallLink removes the copy `triagefactory install` made. It reports
// whether something went wrong (a skip is not a failure).
func removeInstallLink(path string) (failed bool) {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warn: resolve current executable: %v\n", err)
		return true
	}
	msg, err := removeInstalledCopy(path, exe)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "  warn: %v\n", err)
		return true
	case msg != "":
		fmt.Println("  " + msg)
	}
	return false
}

// scheduleDelete asks Windows to delete path at the next restart. It is a var
// so a test can stand in for it: the request is recorded system-wide, and for
// some locations it needs an administrator.
var scheduleDelete = func(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

// removeInstalledCopy deletes the installed copy at path, but only when it is
// a regular file named triagefactory.exe. A running executable cannot be
// deleted on Windows, though it can be renamed, so when path is exePath it is
// renamed to path+".old", scheduled for deletion at the next restart, and the
// message says whether that worked; if not, it tells the user to delete the
// file after this process exits. A skip returns a message and a nil error.
func removeInstalledCopy(path, exePath string) (msg string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("skipped %s: not a regular file", path), nil
	}
	if !strings.EqualFold(filepath.Base(path), installedName) {
		return fmt.Sprintf("skipped %s: not named %s", path, installedName), nil
	}

	old := path + ".old"
	// A leftover from an earlier upgrade; the running copy keeps it locked.
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		msg = fmt.Sprintf(" (%s is still in use; delete it by hand)", old)
	}

	running := false
	if exeInfo, err := os.Stat(exePath); err == nil && os.SameFile(exeInfo, info) {
		running = true
	}
	if running {
		if err := os.Rename(path, old); err != nil {
			return "", fmt.Errorf("move the running copy %s aside: %w", path, err)
		}
		if err := scheduleDelete(old); err != nil {
			return fmt.Sprintf("renamed running install %s to %s; could not schedule its deletion (%v), so delete it after this command exits%s", path, old, err, msg), nil
		}
		return fmt.Sprintf("renamed running install %s to %s; Windows will delete it at the next restart, or delete it yourself after this command exits%s", path, old, msg), nil
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("remove %s: %w", path, err)
	}
	// Drop the install directory too when nothing else is in it.
	if dir := filepath.Dir(path); strings.EqualFold(filepath.Base(dir), "triagefactory") {
		_ = os.Remove(dir)
	}
	return fmt.Sprintf("removed installed copy %s%s", path, msg), nil
}
