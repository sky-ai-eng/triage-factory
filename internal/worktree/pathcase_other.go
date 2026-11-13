//go:build !windows

package worktree

import "os"

// longTempDir is os.TempDir(): only Windows has a second spelling of a path.
func longTempDir() string { return os.TempDir() }

// longPathName is the identity off Windows.
func longPathName(p string) string { return p }

// sameFileName compares two file names exactly.
func sameFileName(a, b string) bool { return a == b }
