//go:build !linux

package worktree

import (
	"os"
	"path/filepath"
)

// readFileConfined falls back to a plain read off Linux: openat2/RESOLVE_BENEATH
// is Linux-only, and the sandbox — hence the cross-uid boundary this guards — is
// Linux-only too, so no confinement is reachable here.
func readFileConfined(root, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, rel))
}

func openFileConfined(root, rel string) (*os.File, error) {
	return os.Open(filepath.Join(root, rel))
}
