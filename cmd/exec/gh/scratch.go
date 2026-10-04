package gh

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// conversationRootEnv names the run root the spawner exports to every
// delegated run. Inside the sandbox it is already translated to /work, so the
// value is a path this process can reach.
const conversationRootEnv = "TRIAGE_FACTORY_CONVERSATION_ROOT"

// runRoot returns the run root every verb that writes under _tfac writes
// into. The run root is TF's directory; the checkouts inside it belong to the
// customer's repos, so scratch output never resolves from the current
// directory, which is usually one of those checkouts. The snapshot that carries
// _tfac across steps reads only <run root>/_tfac, and the paths the GitHub tools
// prompt names are fixed under it.
//
// Fails when the variable is unset or relative: these verbs only make sense
// inside a delegated run, and guessing a root would put their output where no
// later step looks for it.
func runRoot() (string, error) {
	root := os.Getenv(conversationRootEnv)
	if root == "" {
		return "", fmt.Errorf("%s is not set: this verb writes its output under the run root's _tfac/ and runs only inside a delegated run", conversationRootEnv)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", conversationRootEnv, root)
	}
	return filepath.Clean(root), nil
}

// safeScratchSubdir resolves root/<parts...> with a symlink safety check at
// every path component that already exists. It's the shared dest-resolution
// primitive behind both `actions download-logs` (ci-logs/<run_id>) and
// `pr diff` (pr-diffs/<owner>__<repo>__<number>): any command that
// RemoveAll / MkdirAll / writes under _tfac must route through it so a
// symlinked component (accidental or malicious) can't redirect those
// filesystem operations outside the run root. root itself is not checked —
// it comes from the spawner, not from anything the agent can create.
//
// Components that don't exist yet are fine — the caller's MkdirAll creates
// them, and there's nothing past the first missing component to symlink-check.
// Pre-existing real directories are fine (the "second capture in the same run"
// case). Only pre-existing symlinks (rejected) and non-directory files
// (rejected with a clearer message than MkdirAll would give) fail.
//
// There's an inherent TOCTOU race between this check and the subsequent
// filesystem operations, but for our threat model (accidental pre-existing
// symlinks in a run tree, not a live attacker with filesystem primitives)
// this is sufficient. Defending against a live race would require *at
// syscalls with O_NOFOLLOW on every path component, which is overkill.
func safeScratchSubdir(root string, parts ...string) (string, error) {
	// Each part must be a single path segment. This is a defense-in-depth
	// backstop to the validation callers do on their own inputs (e.g.
	// owner/repo via splitOwnerRepoStr): without it a "../" or separator in
	// any component would let filepath.Join + Clean escape root, and the
	// caller's RemoveAll/MkdirAll would then act outside the run root.
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\`) {
			return "", fmt.Errorf("invalid scratch path component %q", p)
		}
	}
	current := root
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
			return "", fmt.Errorf("refusing to use symlinked path component %s (resolves outside the run root)", current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path component %s exists but is not a directory", current)
		}
	}
	// Re-derive the full path from root+parts rather than returning `current`:
	// the loop breaks early at the first non-existent component, so `current`
	// holds only the path-so-far, not the complete destination.
	return filepath.Join(append([]string{root}, parts...)...), nil
}
