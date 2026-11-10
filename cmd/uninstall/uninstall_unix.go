//go:build !windows

package uninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

const (
	// installArtifact names what `triagefactory install` leaves behind, for
	// the "about to remove" list.
	installArtifact = "install symlink"

	// binaryStaysNote and binaryDoneNote tell the user how the binary itself
	// goes, which uninstall never removes here.
	binaryStaysNote = "The binary itself stays — remove it with `brew uninstall triagefactory` (or by hand for source builds)."
	binaryDoneNote  = "To remove the binary, run `brew uninstall triagefactory`."
)

// defaultInstallLink mirrors the destination logic in cmd/install. We
// don't try to discover non-default locations: if the user passed
// --dest somewhere weird at install time, they know where it went and
// can remove it themselves. False positives would be worse than false
// negatives here.
func defaultInstallLink() string {
	if runtime.GOOS == "darwin" {
		return "/usr/local/bin/triagefactory"
	}
	// Binary symlink location (~/.local/bin), not TF state — resolved via
	// ExpandHome so os.UserHomeDir stays confined to internal/paths.
	link, err := paths.ExpandHome("~/.local/bin/triagefactory")
	if err != nil {
		return ""
	}
	return link
}

// removeInstallLink removes the symlink `triagefactory install` left at
// linkPath, only when it points at the running executable. It reports whether
// something went wrong (a skip is not a failure).
func removeInstallLink(linkPath string) (failed bool) {
	info, err := os.Lstat(linkPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warn: inspect %s: %v\n", linkPath, err)
		return true
	}
	if info.Mode()&os.ModeSymlink == 0 {
		fmt.Fprintf(os.Stderr, "  warn: skip removing %s: path exists but is not a symlink\n", linkPath)
		return false
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warn: inspect symlink target %s: %v\n", linkPath, err)
		return true
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warn: resolve current executable: %v\n", err)
		return true
	}
	resolvedTarget := target
	if p, err := filepath.EvalSymlinks(target); err == nil {
		resolvedTarget = p
	}

	resolvedExe := exePath
	if p, err := filepath.EvalSymlinks(exePath); err == nil {
		resolvedExe = p
	}

	if filepath.Clean(resolvedTarget) != filepath.Clean(resolvedExe) {
		fmt.Fprintf(os.Stderr, "  warn: skip removing %s: symlink points to %q, not the current executable %q\n", linkPath, target, exePath)
		return false
	}
	if err := os.Remove(linkPath); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: remove %s: %v (try: sudo rm %q)\n", linkPath, err, linkPath)
		return true
	}
	fmt.Printf("  removed install symlink %s\n", linkPath)
	return false
}
