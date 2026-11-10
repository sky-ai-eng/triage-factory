//go:build windows

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinary writes a stand-in for the running executable.
func fakeBinary(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "triagefactory.exe")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallCopy_FreshInstallCopiesBinary(t *testing.T) {
	src := fakeBinary(t, "v1")
	target := filepath.Join(t.TempDir(), "Programs", "triagefactory", "triagefactory.exe")

	res, err := installCopy(src, target, false)
	if err != nil || res.alreadyRunning {
		t.Fatalf("installCopy = %+v, %v", res, err)
	}
	if got := readFile(t, target); got != "v1" {
		t.Errorf("target content = %q, want v1", got)
	}
	if fi, err := os.Lstat(target); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("target must be a regular file, got %v, %v", fi, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 1 {
		t.Errorf("destination dir should hold only the binary, got %d entries", len(entries))
	}
}

func TestInstallCopy_UpgradeReplacesAndCleansOld(t *testing.T) {
	target := filepath.Join(t.TempDir(), "triagefactory.exe")
	if _, err := installCopy(fakeBinary(t, "v1"), target, false); err != nil {
		t.Fatal(err)
	}
	// A stale .old from an earlier upgrade must not block the next one.
	if err := os.WriteFile(target+".old", []byte("ancient"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installCopy(fakeBinary(t, "v2"), target, false); err != nil {
		t.Fatalf("upgrade without --force: %v", err)
	}
	if got := readFile(t, target); got != "v2" {
		t.Errorf("target content = %q, want v2", got)
	}
	if _, err := os.Stat(target + ".old"); !os.IsNotExist(err) {
		t.Errorf("%s.old should be gone, stat err = %v", target, err)
	}
}

// A previous .old that is still running cannot be deleted; the upgrade takes
// the next numbered name instead of failing, and unrelated files are untouched.
func TestInstallCopy_RunningOldCopyDoesNotBlockUpgrade(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "triagefactory.exe")
	if _, err := installCopy(fakeBinary(t, "v1"), target, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".old", []byte("running"), 0o755); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "triagefactory.exe.oldnotes")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// os.Open shares read/write but not delete, so the file cannot be removed
	// while held, as with a running image.
	held, err := os.Open(target + ".old")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := installCopy(fakeBinary(t, "v2"), target, false); err != nil {
		t.Fatalf("upgrade with a running .old: %v", err)
	}
	if got := readFile(t, target); got != "v2" {
		t.Errorf("target content = %q, want v2", got)
	}
	if got := readFile(t, target+".old"); got != "running" {
		t.Errorf("the running copy was disturbed: %q", got)
	}
	if got := readFile(t, unrelated); got != "keep" {
		t.Errorf("an unrelated file was touched: %q", got)
	}

	// Once nothing holds it, the next install cleans both up.
	held.Close()
	if _, err := installCopy(fakeBinary(t, "v3"), target, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{target + ".old", target + ".old.1"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat err = %v", p, err)
		}
	}
}

// Only target.old and target.old.<digits> are ours to delete; a file a person
// named target.old.bak survives an upgrade.
func TestInstallCopy_UpgradeKeepsLookalikeFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "triagefactory.exe")
	if _, err := installCopy(fakeBinary(t, "v1"), target, false); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "triagefactory.exe.old.bak")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	numbered := target + ".old.7"
	if err := os.WriteFile(numbered, []byte("ours"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installCopy(fakeBinary(t, "v2"), target, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, keep); got != "mine" {
		t.Errorf("triagefactory.exe.old.bak was touched: %q", got)
	}
	if _, err := os.Stat(numbered); !os.IsNotExist(err) {
		t.Errorf("a numbered aside should be cleaned up, stat err = %v", err)
	}
}

func TestInstallCopy_FailedSwapKeepsOriginalAndLeavesNoTemp(t *testing.T) {
	// os.Open shares read/write but not delete, so the rename-aside is refused
	// the way it is when an antivirus scanner holds the file.
	dir := t.TempDir()
	target := filepath.Join(dir, "triagefactory.exe")
	if _, err := installCopy(fakeBinary(t, "v1"), target, false); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := installCopy(fakeBinary(t, "v2"), target, false); err == nil {
		t.Fatal("expected the swap to fail while the target is held open")
	}
	if got := readFile(t, target); got != "v1" {
		t.Errorf("original damaged by a failed install: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %d entries in %s", len(entries), dir)
	}
}

func TestInstallCopy_SameFileIsNoOp(t *testing.T) {
	src := fakeBinary(t, "running")
	res, err := installCopy(src, src, false)
	if err != nil || !res.alreadyRunning {
		t.Fatalf("installCopy onto itself = %+v, %v; want alreadyRunning", res, err)
	}
	if got := readFile(t, src); got != "running" {
		t.Errorf("source was modified: %q", got)
	}
}

func TestInstallCopy_OtherNamedFileNeedsForce(t *testing.T) {
	target := filepath.Join(t.TempDir(), "tool.exe")
	if err := os.WriteFile(target, []byte("not ours"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := fakeBinary(t, "v1")
	if _, err := installCopy(src, target, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want a --force refusal", err)
	}
	if got := readFile(t, target); got != "not ours" {
		t.Errorf("refused install modified the file: %q", got)
	}
	if _, err := installCopy(src, target, true); err != nil {
		t.Fatalf("with --force: %v", err)
	}
	if got := readFile(t, target); got != "v1" {
		t.Errorf("target content = %q, want v1", got)
	}
}

func TestInstallCopy_RefusesDirectoryAtTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "triagefactory.exe")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installCopy(fakeBinary(t, "v1"), target, true); err == nil {
		t.Fatal("expected an error when the target is a directory")
	}
}

func TestOnPath_WindowsCaseAndTrailingSeparator(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", `C:\elsewhere;`+strings.ToUpper(dir)+`\;`)
	if !onPath(dir) {
		t.Errorf("onPath(%q) = false; entries compare case-insensitively and ignore a trailing separator", dir)
	}
	t.Setenv("PATH", `C:\elsewhere;;"`+dir+`"`)
	if !onPath(dir) {
		t.Errorf("onPath(%q) = false for a quoted entry", dir)
	}
	t.Setenv("PATH", `C:\elsewhere;`)
	if onPath(dir) {
		t.Errorf("onPath(%q) = true, want false", dir)
	}
}

func TestPathHint(t *testing.T) {
	got := pathHint(`C:\Users\a b\AppData\Local\Programs\triagefactory`)
	want := `[Environment]::SetEnvironmentVariable("Path", "C:\Users\a b\AppData\Local\Programs\triagefactory;" + [Environment]::GetEnvironmentVariable("Path","User"), "User")`
	if got != want {
		t.Errorf("pathHint =\n %s\nwant\n %s", got, want)
	}
	if h := pathHint(`C:\a$b`); !strings.Contains(h, "C:\\a`$b;") {
		t.Errorf("`$` must be escaped inside the PowerShell string: %s", h)
	}
}
