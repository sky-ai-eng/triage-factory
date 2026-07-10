//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func chownRunTree(ctx context.Context, root, subpath string) error {
	return defaultOps.ChownRunTree(ctx, root, subpath)
}

func removeRunTree(ctx context.Context, path string) error {
	return defaultOps.RemoveRunTree(ctx, path)
}

func captureRunDelta(ctx context.Context, worktree string) ([]byte, error) {
	return defaultOps.CaptureRunDelta(ctx, worktree)
}

// runTreeOwnerExtraUID widens allowedRunTreeOwner beyond this process's
// own uid: the broker runs as root but operates on trees the (different,
// unprivileged) orchestrator created, so runBroker sets this from its
// --orchestrator-uid flag at boot. -1 (the default, in-process case)
// adds nothing — there, the tree creator IS this process.
var runTreeOwnerExtraUID = -1

// SetRunTreeOwnerUID registers the orchestrator's uid as an accepted
// run-tree owner for the validation below. Called once by the cap-broker
// subcommand at boot, before serving; never called in-process.
func SetRunTreeOwnerUID(uid int) { runTreeOwnerExtraUID = uid }

// allowedRunTreeOwner is the ownership precondition on every run-tree
// operation at this privileged boundary: the tree (and, for chown, every
// entry in it) must already belong to an identity that legitimately
// produces run trees — the process that created it (this uid in-process;
// the registered orchestrator uid when brokered) or the sandbox identity
// itself (idempotent re-chown after a resume; teardown of a tree a run
// wrote into). Anything else — /etc, another service's state — fails
// closed. This, not path shape, is the real teeth: even a validly-shaped
// path can't make the privileged side touch a tree the orchestrator
// doesn't already own.
func allowedRunTreeOwner(uid uint32) bool {
	if int(uid) == os.Getuid() || uid == WorktreeUID {
		return true
	}
	return runTreeOwnerExtraUID >= 0 && int(uid) == runTreeOwnerExtraUID
}

// validateRunTreeShape is the string-only half of the run-tree path gate
// — checks that need no filesystem access, so callers can run them even
// on a path that may legitimately not exist (RemoveRunTree's idempotent
// missing-path case must still reject malformed input rather than
// blessing it as a silent no-op). Requires an absolute, clean path at
// least two components deep: every legitimate run tree lives under a
// parent like /tmp/<runs-dir>/ or a state-root subdirectory — "/",
// "/tmp", "/data" themselves can never be run trees.
func validateRunTreeShape(op, path string) error {
	if !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return fmt.Errorf("sandbox: %s: path %q must be absolute and clean", op, path)
	}
	if path == "/" || filepath.Dir(path) == "/" {
		return fmt.Errorf("sandbox: %s: path %q too shallow for a run tree", op, path)
	}
	return nil
}

// validateRunTreeRoot is the full path gate every run-tree op runs before
// touching anything, in the spirit of ValidateLaunchParams: reject
// anything a compromised orchestrator could use to steer a privileged
// chown/remove somewhere surprising. The shape rules above, plus:
// resolving to itself (no symlinked parents redirecting the walk), an
// actual directory, owned per allowedRunTreeOwner.
func validateRunTreeRoot(op, path string) (os.FileInfo, error) {
	if err := validateRunTreeShape(op, path); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s: resolve %s: %w", op, path, err)
	}
	if resolved != path {
		return nil, fmt.Errorf("sandbox: %s: path %q resolves elsewhere (%q) — symlinked components are not accepted at this boundary", op, path, resolved)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s: lstat %s: %w", op, path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox: %s: %s is not a directory", op, path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("sandbox: %s: no stat for %s", op, path)
	}
	if !allowedRunTreeOwner(st.Uid) {
		return nil, fmt.Errorf("sandbox: %s: %s is owned by uid %d, not a run-tree owner", op, path, st.Uid)
	}
	return info, nil
}

// ChownRunTree implements the ownership hand-off in-process — the same
// recursive Lchown that lived in agentproc.chownWorktreeForSandbox,
// behind the boundary validation above.
//
// SECURITY: uses os.Lchown (not os.Chown) so a symlink inside the tree
// can't redirect the chown to a host file outside it. filepath.Walk does
// not follow symlinks during the walk itself, so the recursion stays
// inside the tree; the per-entry Lchown changes the link's own owner
// rather than the target's. On top of that, every entry's CURRENT owner
// is checked against allowedRunTreeOwner before it's touched — an entry
// planted with a foreign owner fails the whole operation closed rather
// than being silently re-owned.
func (hostOps) ChownRunTree(ctx context.Context, root, subpath string) error {
	if _, err := validateRunTreeRoot("chown run tree", root); err != nil {
		return err
	}

	target := root
	if subpath != "" {
		rel := filepath.Clean(subpath)
		if filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("sandbox: chown run tree: subpath %q escapes root %q", subpath, root)
		}
		// The mid-run `workspace add` case: the intermediate directories
		// the checkout minted between root and the new tree must at least
		// be traversable-and-sandbox-owned so the agent can later add
		// sibling checkouts, remove its tree, etc. Shallow — their other
		// contents were chowned at run start.
		dir := root
		segs := strings.Split(rel, string(filepath.Separator))
		for _, seg := range segs[:len(segs)-1] {
			dir = filepath.Join(dir, seg)
			if err := lchownRunTreeEntry(dir); err != nil {
				return fmt.Errorf("sandbox: chown run tree: %w", err)
			}
		}
		target = filepath.Join(root, rel)
	}

	err := filepath.Walk(target, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return lchownRunTreeEntry(path)
	})
	if err != nil {
		return fmt.Errorf("sandbox: chown run tree: %w", err)
	}
	return nil
}

// lchownRunTreeEntry re-owns one entry to the sandbox identity, after
// checking its current owner is one this boundary hands over at all.
func lchownRunTreeEntry(path string) error {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if !allowedRunTreeOwner(st.Uid) {
		return fmt.Errorf("%s is owned by uid %d, not a run-tree owner", path, st.Uid)
	}
	if err := os.Lchown(path, WorktreeUID, WorktreeGID); err != nil {
		return fmt.Errorf("lchown %s: %w", path, err)
	}
	return nil
}

// RemoveRunTree implements the teardown half in-process: boundary
// validation on the tree's top level, then a plain os.RemoveAll — the
// contents go regardless of what uids the run left inside (agent-created
// files arrive via the privileged gofer and can carry modes the
// orchestrator could never unlink through, which is exactly why this op
// lives on the privileged side).
func (hostOps) RemoveRunTree(ctx context.Context, path string) error {
	// Shape first, existence second: a malformed path is a caller bug (or
	// hostile input) and must error even when nothing exists there — only
	// a well-formed-but-absent tree is the idempotent no-op the
	// os.RemoveAll callers this replaces relied on.
	if err := validateRunTreeShape("remove run tree", path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	}
	if _, err := validateRunTreeRoot("remove run tree", path); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("sandbox: remove run tree: %w", err)
	}
	return nil
}
