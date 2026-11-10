//go:build !windows

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// run installs src at target as a symlink, so later builds of the binary
// propagate without re-running install.
func run(src, target string, force bool) {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		// Almost certainly permission denied on /usr/local/bin without
		// sudo. Print the explicit command rather than escalating.
		fmt.Fprintf(os.Stderr, "triagefactory install: cannot create %s: %v\n", filepath.Dir(target), err)
		fmt.Fprintf(os.Stderr, "\nTry:\n  sudo ln -sf %q %q\n", src, target)
		os.Exit(1)
	}

	if existing, err := os.Lstat(target); err == nil {
		// Resolve the existing symlink so we can tell the user where
		// it currently points and skip the rewrite if it's already us.
		if existing.Mode()&os.ModeSymlink != 0 {
			if cur, err := os.Readlink(target); err == nil && cur == src {
				fmt.Printf("triagefactory: already installed at %s -> %s\n", target, src)
				return
			}
		}
		if !force {
			fmt.Fprintf(os.Stderr, "triagefactory install: %s already exists; pass --force to overwrite\n", target)
			os.Exit(1)
		}
		if err := os.Remove(target); err != nil {
			// `rm -rf` so the hint works whether target is a regular
			// file, a symlink, or a directory — `os.Remove` fails on
			// non-empty dirs with "is a directory", and a plain
			// `sudo rm` would fail the same way.
			fmt.Fprintf(os.Stderr, "triagefactory install: cannot remove existing %s: %v\n", target, err)
			fmt.Fprintf(os.Stderr, "\nTry:\n  sudo rm -rf %q && sudo ln -s %q %q\n", target, src, target)
			os.Exit(1)
		}
	}

	if err := os.Symlink(src, target); err != nil {
		fmt.Fprintf(os.Stderr, "triagefactory install: cannot link %s -> %s: %v\n", target, src, err)
		fmt.Fprintf(os.Stderr, "\nTry:\n  sudo ln -s %q %q\n", src, target)
		os.Exit(1)
	}

	fmt.Printf("triagefactory: installed %s -> %s\n", target, src)
	if !onPath(filepath.Dir(target)) {
		fmt.Fprintf(os.Stderr, "\nNote: %s is not on your PATH. Add it to your shell rc:\n", filepath.Dir(target))
		fmt.Fprintf(os.Stderr, "  export PATH=%q:$PATH\n", filepath.Dir(target))
	}
}

// defaultDestination picks a sensible install path per OS. macOS gets
// /usr/local/bin (the conventional spot for user-installed binaries,
// usually on PATH out of the box, requires sudo). Linux gets the XDG
// user-bin path which doesn't need sudo and is typically already on
// PATH for modern distros.
func defaultDestination() string {
	if runtime.GOOS == "darwin" {
		return "/usr/local/bin/triagefactory"
	}
	return "~/.local/bin/triagefactory"
}

// pathEntryIs reports whether the $PATH entry names the directory abs
// (already absolute). Exact comparison: Unix paths are case-sensitive.
func pathEntryIs(entry, abs string) bool {
	pAbs, err := filepath.Abs(entry)
	return err == nil && pAbs == abs
}
