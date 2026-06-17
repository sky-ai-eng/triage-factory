package gh

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// safeScratchSubdir resolves cwd/<parts...> with a symlink safety check at
// every path component that already exists. It's the shared dest-resolution
// primitive behind both `actions download-logs` (ci-logs/<run_id>) and
// `pr diff` (pr-diffs/<owner>__<repo>__<number>/<sha>): any command that
// RemoveAll / MkdirAll / writes under _scratch must route through it so a
// symlinked component (accidental or malicious) can't redirect those
// filesystem operations outside the working directory.
//
// Components that don't exist yet are fine — the caller's MkdirAll creates
// them, and there's nothing past the first missing component to symlink-check.
// Pre-existing real directories are fine (the "run #2 on the same cwd" case).
// Only pre-existing symlinks (rejected) and non-directory files (rejected with
// a clearer message than MkdirAll would give) fail.
//
// There's an inherent TOCTOU race between this check and the subsequent
// filesystem operations, but for our threat model (accidental pre-existing
// symlinks in a worktree, not a live attacker with filesystem primitives)
// this is sufficient. Defending against a live race would require *at
// syscalls with O_NOFOLLOW on every path component, which is overkill.
func safeScratchSubdir(cwd string, parts ...string) (string, error) {
	// Each part must be a single path segment. This is a defense-in-depth
	// backstop to the validation callers do on their own inputs (e.g.
	// owner/repo via splitOwnerRepoStr): without it a "../" or separator in
	// any component would let filepath.Join + Clean escape cwd, and the
	// caller's RemoveAll/MkdirAll would then act outside the worktree.
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\`) {
			return "", fmt.Errorf("invalid scratch path component %q", p)
		}
	}
	current := cwd
	for _, p := range parts {
		current = filepath.Join(current, p)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// Nothing past this point exists yet — the rest of the
				// path is created by MkdirAll, so there's nothing left
				// to symlink-check.
				break
			}
			return "", fmt.Errorf("stat path component %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to use symlinked path component %s (resolves outside the worktree)", current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path component %s exists but is not a directory", current)
		}
	}
	return filepath.Join(append([]string{cwd}, parts...)...), nil
}
