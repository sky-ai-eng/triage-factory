// Package install implements the `triagefactory install` CLI
// subcommand that puts the currently-running binary on the user's PATH
// so the `triagefactory` CLI works from any terminal without a full
// path. On macOS and Linux it is a symlink (not a copy) so future builds
// of the binary auto-propagate without re-running install.
//
// Default destinations:
//   - macOS:  /usr/local/bin/triagefactory     (typically requires sudo)
//   - Linux:  ~/.local/bin/triagefactory       (XDG userland; usually
//     already on PATH)
//   - Windows: %LOCALAPPDATA%\Programs\triagefactory\triagefactory.exe
//     (a copy, not a link: symlinks need Developer Mode or elevation on
//     Windows, so re-run install after upgrading to refresh it)
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

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// Handle dispatches the install subcommand.
func Handle(args []string) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dest := fs.String("dest", "", "destination path (default: /usr/local/bin/triagefactory on macOS, ~/.local/bin/triagefactory on Linux, %LOCALAPPDATA%\\Programs\\triagefactory\\triagefactory.exe on Windows)")
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
		if target == "" {
			fail("cannot resolve the default destination; pass --dest")
		}
	}
	target, err = paths.ExpandHome(target)
	if err != nil {
		fail("expand destination: %v", err)
	}

	run(src, target, *force)
}

// onPath returns true iff dir is one of the directories listed in
// $PATH. Used after install to warn the user when the destination
// they (or we) picked won't be found by their shell. How two entries
// compare is per-platform (pathEntryIs): exact on Unix, case-insensitive
// on Windows.
func onPath(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if pathEntryIs(p, abs) {
			return true
		}
	}
	return false
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory install: "+format+"\n", args...)
	os.Exit(1)
}
