//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxAsideFiles bounds how many target.old.<n> names AsidePath tries.
const maxAsideFiles = 100

// isAsideName reports whether name is an aside of the file called base:
// exactly base.old, or base.old.<digits>. Windows file names are compared
// case-insensitively. Anything else that merely starts with base.old (a
// base.old.bak a person made) is not ours.
func isAsideName(base, name string) bool {
	prefix := strings.ToLower(base) + ".old"
	name = strings.ToLower(name)
	if name == prefix {
		return true
	}
	n, ok := strings.CutPrefix(name, prefix+".")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// RemoveAsides deletes the earlier asides of target (target.old and
// target.old.<n>) and returns those that could not be deleted. A copy that is
// still running cannot be deleted, and is reported rather than failing the
// caller.
func RemoveAsides(target string) (remaining []string) {
	dir, base := filepath.Split(target)
	entries, err := os.ReadDir(filepath.Clean(dir))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !isAsideName(base, e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			remaining = append(remaining, p)
		}
	}
	return remaining
}

// AsidePath returns the name an existing target is renamed to so that it can be
// replaced or removed while it runs. It first deletes earlier asides; the
// result is target.old when that is free and otherwise the first free
// target.old.<n>, so a running previous copy never blocks the caller.
func AsidePath(target string) (string, error) {
	RemoveAsides(target)
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
