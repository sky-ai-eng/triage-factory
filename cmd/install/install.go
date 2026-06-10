// Package install implements the `triagefactory install` CLI
// subcommand that puts the currently-running binary on the user's PATH
// so the `triagefactory` CLI works from any terminal without a full
// path. Implemented as a symlink (not a copy) so future builds of the
// binary auto-propagate without re-running install.
//
// Default destinations:
//   - macOS:  /usr/local/bin/triagefactory     (typically requires sudo)
//   - Linux:  ~/.local/bin/triagefactory       (XDG userland; usually
//     already on PATH)
//
// Override with `--dest /full/path/to/triagefactory` if you keep
// binaries elsewhere. We don't write to non-default locations without
// the explicit flag — a wrong default could surprise users who keep a
// different `triagefactory` link somewhere already.
//
// If the destination needs sudo, we don't escalate ourselves —
// detecting the case and printing the exact command for the user is
// safer than silently invoking sudo from a binary the user just
// installed.
package install

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// Handle dispatches the install subcommand.
func Handle(args []string) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dest := fs.String("dest", "", "destination path (default: /usr/local/bin/triagefactory on macOS, ~/.local/bin/triagefactory on Linux)")
	force := fs.Bool("force", false, "replace an existing file/symlink at the destination")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	src, err := os.Executable()
	if err != nil {
		fail("resolve binary path: %v", err)
	}
	src, err = filepath.Abs(src)
	if err != nil {
		fail("absolute binary path: %v", err)
	}

	target := *dest
	if target == "" {
		target = defaultDestination()
	}
	target, err = paths.ExpandHome(target)
	if err != nil {
		fail("expand destination: %v", err)
	}

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
		if !*force {
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

// onPath returns true iff dir is one of the directories listed in
// $PATH. Used after install to warn the user when the destination
// they (or we) picked won't be found by their shell.
func onPath(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if pAbs, err := filepath.Abs(p); err == nil && pAbs == abs {
			return true
		}
	}
	return false
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory install: "+format+"\n", args...)
	os.Exit(1)
}
