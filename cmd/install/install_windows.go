//go:build windows

package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/fsutil"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// installedName is the file name install writes. An existing file of another
// name is never replaced without --force.
const installedName = "triagefactory.exe"

// run copies src to target. Windows has no usable unprivileged symlink, so the
// install is a copy and goes stale when the source binary is rebuilt; the
// closing message says to re-run install after an upgrade.
func run(src, target string, force bool) {
	res, err := installCopy(src, target, force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "triagefactory install: %v\n", err)
		os.Exit(1)
	}
	if res.alreadyRunning {
		fmt.Printf("triagefactory: already installed at %s (this is the running binary)\n", target)
	} else {
		fmt.Printf("triagefactory: installed %s (copy of %s)\n", target, src)
		fmt.Println("Re-run `triagefactory install` after upgrading to refresh this copy.")
	}
	if !onPath(filepath.Dir(target)) {
		fmt.Fprintf(os.Stderr, "\nNote: %s is not on your PATH. Add it for your user in PowerShell, then open a new terminal:\n", filepath.Dir(target))
		fmt.Fprintf(os.Stderr, "  %s\n", pathHint(filepath.Dir(target)))
	}
}

type installResult struct {
	// alreadyRunning is true when target is the binary that is running, so
	// nothing was written.
	alreadyRunning bool
}

// installCopy puts a copy of src at target. The copy is written to a temp file
// in the destination directory and renamed into place, so a failed copy never
// leaves a truncated binary at target. An existing target is renamed to
// target.old first (Windows allows renaming a running executable but not
// overwriting it), and that file is deleted best-effort afterwards; it is
// removed on the next install if the delete fails because that copy is still
// running. When an earlier target.old is itself still running, the new one
// takes the name target.old.<n> instead of failing the install. Renames retry
// briefly when another process (a scanner, an indexer) holds the file.
func installCopy(src, target string, force bool) (installResult, error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return installResult{}, fmt.Errorf("cannot create %s: %w", dir, err)
	}

	existing, statErr := os.Lstat(target)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return installResult{}, fmt.Errorf("inspect %s: %w", target, statErr)
	}
	if exists {
		srcInfo, err := os.Stat(src)
		if err != nil {
			return installResult{}, fmt.Errorf("stat %s: %w", src, err)
		}
		if os.SameFile(srcInfo, existing) {
			return installResult{alreadyRunning: true}, nil
		}
		if !existing.Mode().IsRegular() {
			return installResult{}, fmt.Errorf("%s exists and is not a regular file; remove it by hand", target)
		}
		// Replacing our own earlier copy is what re-running after an upgrade
		// means, so it needs no flag; anything else needs --force.
		if !force && !strings.EqualFold(filepath.Base(target), installedName) {
			return installResult{}, fmt.Errorf("%s already exists; pass --force to overwrite", target)
		}
	}

	tmp, err := os.CreateTemp(dir, "triagefactory-install-*.tmp")
	if err != nil {
		return installResult{}, fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if err := copyInto(tmp, src); err != nil {
		_ = os.Remove(tmpName) // best-effort; the error returned is the real failure
		return installResult{}, err
	}

	var old string
	if exists {
		var err error
		if old, err = asidePath(target); err != nil {
			_ = os.Remove(tmpName) // best-effort; the error returned is the real failure
			return installResult{}, err
		}
		if err := fsutil.RenameReplace(target, old); err != nil {
			_ = os.Remove(tmpName) // best-effort; the error returned is the real failure
			return installResult{}, fmt.Errorf("move existing %s aside: %w", target, err)
		}
	}
	if err := fsutil.RenameReplace(tmpName, target); err != nil {
		_ = os.Remove(tmpName) // best-effort; the error returned is the real failure
		if exists {
			if rerr := fsutil.RenameReplace(old, target); rerr != nil {
				return installResult{}, fmt.Errorf("install %s: %w (restoring the previous copy from %s also failed: %v)", target, err, old, rerr)
			}
		}
		return installResult{}, fmt.Errorf("install %s: %w", target, err)
	}
	if exists {
		// Best effort: fails harmlessly while the old copy is still running.
		_ = os.Remove(old)
	}
	return installResult{}, nil
}

// maxAsideFiles bounds how many target.old.<n> names asidePath tries.
const maxAsideFiles = 100

// asidePath returns the name the existing target is renamed to. It first tries
// to delete earlier copies, target.old and target.old.<n>: a copy that is still
// running cannot be deleted, and is skipped here rather than failing the
// install. The result is target.old when that is free and otherwise the first
// free target.old.<n>, so a running previous copy never blocks an upgrade.
func asidePath(target string) (string, error) {
	dir, base := filepath.Split(target)
	prefix := strings.ToLower(base + ".old")
	if entries, err := os.ReadDir(filepath.Clean(dir)); err == nil {
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if name == prefix || strings.HasPrefix(name, prefix+".") {
				// Best effort: a copy still in use stays, and is retried next install.
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	for n := 0; n < maxAsideFiles; n++ {
		cand := target + ".old"
		if n > 0 {
			cand = fmt.Sprintf("%s.old.%d", target, n)
		}
		if _, err := os.Lstat(cand); errors.Is(err, os.ErrNotExist) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%s.old and %d numbered copies are all in use; delete them by hand", target, maxAsideFiles-1)
}

// copyInto copies the file at src into the already-open tmp and closes it.
func copyInto(tmp *os.File, src string) (err error) {
	defer func() {
		if cerr := tmp.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close %s: %w", tmp.Name(), cerr)
		}
	}()
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	return nil
}

// defaultDestination is %LOCALAPPDATA%\Programs\triagefactory\triagefactory.exe,
// or "" when the profile directory cannot be resolved (Handle then asks for
// --dest).
func defaultDestination() string {
	p, err := paths.WindowsInstallPath()
	if err != nil {
		return ""
	}
	return p
}

// pathEntryIs reports whether the %PATH% entry names the directory abs
// (already absolute). Windows paths are case-insensitive, entries may carry
// quotes, and a trailing separator is not significant.
func pathEntryIs(entry, abs string) bool {
	entry = strings.Trim(strings.TrimSpace(entry), `"`)
	if entry == "" {
		return false
	}
	pAbs, err := filepath.Abs(entry)
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(pAbs), filepath.Clean(abs))
}

// pathHint is the PowerShell command that prepends dir to the user's PATH.
func pathHint(dir string) string {
	return fmt.Sprintf(`[Environment]::SetEnvironmentVariable("Path", "%s;" + [Environment]::GetEnvironmentVariable("Path","User"), "User")`, psEscape(dir))
}

// psEscape escapes the characters that are live inside a PowerShell
// double-quoted string.
func psEscape(s string) string {
	return strings.NewReplacer("`", "``", `"`, "`\"", "$", "`$").Replace(s)
}
