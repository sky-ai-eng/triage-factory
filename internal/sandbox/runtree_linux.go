//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
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

// runTreeOwnerExtraUID is the orchestrator's uid when this code runs
// inside the broker, and the flag that puts allowedRunTreeOwner into its
// brokered mode. The broker runs as root but operates only on trees the
// (different, unprivileged) orchestrator created, so runBroker sets this
// from its --orchestrator-uid flag at boot; once set, it REPLACES the
// process's own (root) uid as the accepted owner rather than adding to it
// — see allowedRunTreeOwner. -1 (the default, in-process case) leaves the
// process's own uid as the accepted owner, because there the tree creator
// IS this process.
var runTreeOwnerExtraUID = -1

// SetRunTreeOwnerUID registers the orchestrator's uid as the accepted
// run-tree owner for the validation below, switching allowedRunTreeOwner
// into brokered mode. Called once by the cap-broker subcommand at boot,
// before serving; never called in-process.
func SetRunTreeOwnerUID(uid int) { runTreeOwnerExtraUID = uid }

// allowedRunTreeOwner is the ownership precondition on every run-tree
// operation at this privileged boundary: the tree (and, for chown, every
// entry in it) must already belong to an identity that legitimately
// produces run trees, or the sandbox identity itself (idempotent re-chown
// after a resume; teardown of a tree a run wrote into). Anything else —
// /etc/ssl, another service's state — fails closed. This, not path shape,
// is the real teeth: even a validly-shaped path can't make the privileged
// side touch a tree the orchestrator doesn't already own.
//
// Which identity "legitimately produces run trees" is deployment-shaped,
// and the split matters for security:
//
//   - Brokered (runTreeOwnerExtraUID >= 0): this code runs inside the
//     ROOT cap-broker, which never creates run trees itself — only the
//     unprivileged orchestrator does, as runTreeOwnerExtraUID. So the
//     accepted owners are exactly that uid and the sandbox identity;
//     os.Getuid() (0, root) is deliberately NOT trusted here. Trusting it
//     would accept every root-owned directory in the image (/usr/local/bin,
//     /var/lib/..., anything ≥2 components deep) as a "run tree" and hand a
//     compromised, capability-less orchestrator — which still owns and can
//     dial the broker socket — a root-privileged recursive-chown / RemoveAll
//     primitive against host state, reopening exactly the boundary this
//     validation exists to close.
//   - Same-uid (runTreeOwnerExtraUID < 0): the dev/bare-metal case where
//     the broker was self-spawned without --orchestrator-uid, so it runs as
//     the same uid as the orchestrator (no exec-time drop) and IS the one
//     that creates run trees — "owned by me" is the correct legitimacy
//     check, and no separate orchestrator uid exists to register.
func allowedRunTreeOwner(uid uint32) bool {
	if uid == WorktreeUID {
		return true
	}
	if runTreeOwnerExtraUID >= 0 {
		// Brokered: only the registered orchestrator uid — never the
		// broker's own root uid.
		return int(uid) == runTreeOwnerExtraUID
	}
	// In-process: the creating process's own uid.
	return int(uid) == os.Getuid()
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

// validateRunTreeRoot is the full path gate the fallback implementation
// below runs before touching anything, in the spirit of
// ValidateLaunchParams: reject anything a compromised orchestrator could
// use to steer a privileged chown/remove somewhere surprising. The shape
// rules above, plus: resolving to itself (no symlinked parents redirecting
// the walk), an actual directory, owned per allowedRunTreeOwner.
//
// This is a path-then-separate-syscall check — exactly the TOCTOU window
// the openat2 implementation below closes by resolving and operating
// through one anchored fd instead. It survives only because it backs
// chownRunTreeFallback / removeRunTreeFallback (pre-5.6-kernel path) and
// CaptureRunDelta, which this ticket does not touch.
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

// openat2Syscall is a seam over unix.Openat2 so tests can force an
// ENOSYS/EOPNOTSUPP response to exercise the kernel-fallback path below
// without needing an actual pre-5.6 kernel.
var openat2Syscall = unix.Openat2

// errOpenat2Unsupported wraps a failure from the openat2 call itself when
// the errno indicates the kernel doesn't implement it (or the flag),
// distinguishing "this kernel can't do kernel-enforced resolution" (fall
// back) from every other openat2 failure (a real rejection to surface).
var errOpenat2Unsupported = errors.New("openat2(RESOLVE_NO_SYMLINKS) unsupported by this kernel")

// runTreeOpenat2Unsupported latches the "this kernel doesn't support
// openat2" finding process-wide, the first time either run-tree op hits
// it, so every later call skips straight to the fallback instead of
// re-probing (and re-erroring) per op.
var runTreeOpenat2Unsupported atomic.Bool

// noteOpenat2Unsupported records the fallback decision (idempotent — a
// CompareAndSwap so concurrent first-callers don't double-log) and warns
// once, naming the kernel-version cause per the decided contract.
func noteOpenat2Unsupported(err error) {
	if runTreeOpenat2Unsupported.CompareAndSwap(false, true) {
		sandboxLog.Warn("run-tree chown/remove: openat2(RESOLVE_NO_SYMLINKS) unsupported by this kernel (need Linux 5.6+); falling back to the EvalSymlinks-based resolution", "err", err)
	}
}

// closeFd best-effort closes a raw fd opened by the helpers below. A close
// failure here is not actionable — the fd's actual work (the chown/unlink
// it anchored) already succeeded or failed on its own terms — so, like the
// rest of this codebase's Close-error handling, it's discarded rather than
// surfaced.
func closeFd(fd int) { _ = unix.Close(fd) }

// openRunTreeAnchor is the one openat2 call per top-level ChownRunTree /
// RemoveRunTree invocation: it resolves path's PARENT with
// RESOLVE_NO_SYMLINKS, so the kernel refuses the open outright if ANY
// component of that parent's path is a symlink, rather than a prior
// EvalSymlinks check merely catching it before a separate operation acts.
// Every op after this point is fd-relative off the returned parent fd (or
// an fd descended from it), never re-resolving a path string, so a
// component swapped for a symlink after this call can't redirect anything
// downstream either.
func openRunTreeAnchor(path string) (parentFd int, base string, err error) {
	parent := filepath.Dir(path)
	fd, err := openat2Syscall(unix.AT_FDCWD, parent, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			return -1, "", fmt.Errorf("%w: %v", errOpenat2Unsupported, err)
		}
		return -1, "", fmt.Errorf("openat2 %s: %w", parent, err)
	}
	return fd, filepath.Base(path), nil
}

// readDirNames lists dirFd's entries (excluding "." and "..") via raw
// getdents on the fd itself, never a path-based readdir — so listing
// can't be steered by a symlink swap after dirFd was opened.
func readDirNames(dirFd int) ([]string, error) {
	var names []string
	buf := make([]byte, 8192)
	for {
		n, err := unix.Getdents(dirFd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return nil, err
		}
		if n <= 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
}

// openat2AnchoredChildDir opens name, anchored off dirFd, as a directory
// fd for further fd-relative descent. O_NOFOLLOW refuses a symlink entry
// outright (ELOOP) rather than following it — a symlink is always a leaf
// at this boundary, never traversed. O_NONBLOCK guards the open itself
// against blocking on a FIFO planted in place of a real directory (moot
// once O_DIRECTORY is honored, but cheap insurance against the open
// syscall's own blocking behavior on unusual entry types).
func openat2AnchoredChildDir(dirFd int, name string) (int, error) {
	return unix.Openat(dirFd, name, unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
}

// ChownRunTree implements the ownership hand-off: the openat2-anchored
// path below unless this kernel has already been found not to support it,
// in which case chownRunTreeFallback (the original EvalSymlinks-based
// implementation) takes over — permanently, once latched, so a kernel
// that can't do RESOLVE_NO_SYMLINKS isn't re-probed on every run.
func (hostOps) ChownRunTree(ctx context.Context, root, subpath string) error {
	if err := validateRunTreeShape("chown run tree", root); err != nil {
		return err
	}
	if !runTreeOpenat2Unsupported.Load() {
		err := chownRunTreeOpenat2(ctx, root, subpath)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errOpenat2Unsupported):
			noteOpenat2Unsupported(err)
		default:
			return fmt.Errorf("sandbox: chown run tree: %w", err)
		}
	}
	return chownRunTreeFallback(ctx, root, subpath)
}

// chownRunTreeOpenat2 is the kernel-enforced implementation.
// SECURITY: root's resolution is refused by the kernel (not merely a
// prior check) if any component of its parent is a symlink; every op
// after that resolves fd-relative with AT_SYMLINK_NOFOLLOW /
// O_NOFOLLOW, so a symlink swapped in after the anchor opens is chowned
// as the link itself (never followed to its target) and never descended
// into. Every entry's CURRENT owner is checked against
// allowedRunTreeOwner before it's touched, exactly like the fallback's
// lchownRunTreeEntry — an entry planted with a foreign owner fails the
// whole operation closed.
func chownRunTreeOpenat2(ctx context.Context, root, subpath string) error {
	parentFd, base, err := openRunTreeAnchor(root)
	if err != nil {
		return err
	}
	defer closeFd(parentFd)

	rootFd, err := openat2AnchoredChildDir(parentFd, base)
	if err != nil {
		return fmt.Errorf("openat %s: %w", root, err)
	}
	defer closeFd(rootFd)

	var st unix.Stat_t
	if err := unix.Fstat(rootFd, &st); err != nil {
		return fmt.Errorf("fstat %s: %w", root, err)
	}
	if !allowedRunTreeOwner(st.Uid) {
		return fmt.Errorf("%s is owned by uid %d, not a run-tree owner", root, st.Uid)
	}

	if subpath == "" {
		if err := unix.Fchownat(parentFd, base, WorktreeUID, WorktreeGID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("fchownat %s: %w", root, err)
		}
		return chownDirChildren(ctx, rootFd, root)
	}

	// The mid-run `workspace add` case: the intermediate directories the
	// checkout minted between root and the new tree must at least be
	// traversable-and-sandbox-owned so the agent can later add sibling
	// checkouts, remove its tree, etc. — shallow, since their other
	// contents were chowned at run start. The final segment (the new
	// checkout) gets the full recursive treatment. Root itself is NOT
	// chowned by this form — only its descendants.
	rel := filepath.Clean(subpath)
	if filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("chown run tree: subpath %q escapes root %q", subpath, root)
	}
	segs := strings.Split(rel, string(filepath.Separator))

	dirFd := rootFd
	ownDirFd := false
	display := root
	var opErr error
	for i, seg := range segs {
		display = filepath.Join(display, seg)
		if opErr = ctx.Err(); opErr != nil {
			break
		}
		if i == len(segs)-1 {
			opErr = chownEntryRecursive(ctx, dirFd, seg, display)
			break
		}
		if opErr = chownEntryShallow(dirFd, seg, display); opErr != nil {
			break
		}
		childFd, err := openat2AnchoredChildDir(dirFd, seg)
		if err != nil {
			opErr = fmt.Errorf("openat %s: %w", display, err)
			break
		}
		if ownDirFd {
			closeFd(dirFd)
		}
		dirFd = childFd
		ownDirFd = true
	}
	if ownDirFd {
		closeFd(dirFd)
	}
	return opErr
}

// chownEntryShallow re-owns exactly one entry (dirFd/name) to the sandbox
// identity, after checking its current owner is one this boundary hands
// over at all — the fd-relative replacement for lchownRunTreeEntry, used
// where the original code chowned a single intermediate directory without
// recursing into its other contents.
func chownEntryShallow(dirFd int, name, displayPath string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("fstatat %s: %w", displayPath, err)
	}
	if !allowedRunTreeOwner(st.Uid) {
		return fmt.Errorf("%s is owned by uid %d, not a run-tree owner", displayPath, st.Uid)
	}
	if err := unix.Fchownat(dirFd, name, WorktreeUID, WorktreeGID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("fchownat %s: %w", displayPath, err)
	}
	return nil
}

// chownEntryRecursive re-owns dirFd/name and, if it is itself a directory,
// descends (O_NOFOLLOW — a symlink entry ELOOPs and is left as an
// already-chowned leaf, never traversed) and recurses over everything
// inside it.
func chownEntryRecursive(ctx context.Context, dirFd int, name, displayPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("fstatat %s: %w", displayPath, err)
	}
	if !allowedRunTreeOwner(st.Uid) {
		return fmt.Errorf("%s is owned by uid %d, not a run-tree owner", displayPath, st.Uid)
	}
	if err := unix.Fchownat(dirFd, name, WorktreeUID, WorktreeGID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("fchownat %s: %w", displayPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	childFd, err := openat2AnchoredChildDir(dirFd, name)
	if err != nil {
		return fmt.Errorf("openat %s: %w", displayPath, err)
	}
	defer closeFd(childFd)
	return chownDirChildren(ctx, childFd, displayPath)
}

// chownDirChildren lists dirFd's entries and recursively chowns each one.
func chownDirChildren(ctx context.Context, dirFd int, displayPath string) error {
	names, err := readDirNames(dirFd)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", displayPath, err)
	}
	for _, name := range names {
		if err := chownEntryRecursive(ctx, dirFd, name, filepath.Join(displayPath, name)); err != nil {
			return err
		}
	}
	return nil
}

// chownRunTreeFallback is the original recursive Lchown implementation —
// the same one that lived in agentproc.chownWorktreeForSandbox before the
// broker split, kept verbatim as the pre-5.6-kernel fallback path (see
// runTreeOpenat2Unsupported).
//
// SECURITY: uses os.Lchown (not os.Chown) so a symlink inside the tree
// can't redirect the chown to a host file outside it. filepath.Walk does
// not follow symlinks during the walk itself, so the recursion stays
// inside the tree; the per-entry Lchown changes the link's own owner
// rather than the target's. On top of that, every entry's CURRENT owner
// is checked against allowedRunTreeOwner before it's touched — an entry
// planted with a foreign owner fails the whole operation closed rather
// than being silently re-owned.
func chownRunTreeFallback(ctx context.Context, root, subpath string) error {
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

// RemoveRunTree implements the teardown half: the openat2-anchored path
// below unless this kernel has already been found not to support it, in
// which case removeRunTreeFallback (the original os.RemoveAll-based
// implementation) takes over — same latch as ChownRunTree.
func (hostOps) RemoveRunTree(ctx context.Context, path string) error {
	if err := validateRunTreeShape("remove run tree", path); err != nil {
		return err
	}
	if !runTreeOpenat2Unsupported.Load() {
		err := removeRunTreeOpenat2(ctx, path)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errOpenat2Unsupported):
			noteOpenat2Unsupported(err)
		default:
			return fmt.Errorf("sandbox: remove run tree: %w", err)
		}
	}
	return removeRunTreeFallback(ctx, path)
}

// removeRunTreeOpenat2 is the kernel-enforced implementation: path's
// resolution is refused by the kernel (not merely a prior check) if any
// component of its parent is a symlink; every op after that is
// fd-relative (Fstatat/Openat with AT_SYMLINK_NOFOLLOW/O_NOFOLLOW,
// Unlinkat for the actual removal) — the contents go regardless of what
// uids the run left inside (agent-created files arrive via the privileged
// gofer and can carry modes the orchestrator could never unlink through,
// which is exactly why this op lives on the privileged side), matching
// the fallback's no-interior-ownership-check behavior exactly. A missing
// tree — either path itself, or its parent — is the idempotent no-op
// success the os.RemoveAll callers this replaces relied on.
func removeRunTreeOpenat2(ctx context.Context, path string) error {
	parentFd, base, err := openRunTreeAnchor(path)
	if err != nil {
		if errors.Is(err, errOpenat2Unsupported) {
			return err
		}
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer closeFd(parentFd)

	pathFd, err := openat2AnchoredChildDir(parentFd, base)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("openat %s: %w", path, err)
	}

	var st unix.Stat_t
	if statErr := unix.Fstat(pathFd, &st); statErr != nil {
		closeFd(pathFd)
		return fmt.Errorf("fstat %s: %w", path, statErr)
	}
	if !allowedRunTreeOwner(st.Uid) {
		closeFd(pathFd)
		return fmt.Errorf("%s is owned by uid %d, not a run-tree owner", path, st.Uid)
	}
	if err := ctx.Err(); err != nil {
		closeFd(pathFd)
		return err
	}

	err = removeDirChildren(ctx, pathFd, path)
	closeFd(pathFd)
	if err != nil {
		return err
	}

	if err := unix.Unlinkat(parentFd, base, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("unlinkat(rmdir) %s: %w", path, err)
	}
	return nil
}

// removeDirChildren empties dirFd's directory contents: a directory entry
// is emptied via recursion before AT_REMOVEDIR unlinks the now-empty
// directory itself; anything else is unlinked directly. No ownership
// check here — matching removeRunTreeFallback's os.RemoveAll, which never
// inspected interior uids either.
func removeDirChildren(ctx context.Context, dirFd int, displayPath string) error {
	names, err := readDirNames(dirFd)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", displayPath, err)
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		entryPath := filepath.Join(displayPath, name)
		var st unix.Stat_t
		if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("fstatat %s: %w", entryPath, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			childFd, err := openat2AnchoredChildDir(dirFd, name)
			if err != nil {
				return fmt.Errorf("openat %s: %w", entryPath, err)
			}
			err = removeDirChildren(ctx, childFd, entryPath)
			closeFd(childFd)
			if err != nil {
				return err
			}
			if err := unix.Unlinkat(dirFd, name, unix.AT_REMOVEDIR); err != nil {
				return fmt.Errorf("unlinkat(rmdir) %s: %w", entryPath, err)
			}
			continue
		}
		if err := unix.Unlinkat(dirFd, name, 0); err != nil {
			return fmt.Errorf("unlinkat %s: %w", entryPath, err)
		}
	}
	return nil
}

// removeRunTreeFallback is the original os.RemoveAll-based implementation,
// kept verbatim as the pre-5.6-kernel fallback path (see
// runTreeOpenat2Unsupported). Shape is already validated by the caller
// (RemoveRunTree); existence is checked here, separately from ownership,
// so a malformed path still errors even when nothing exists there — only
// a well-formed-but-absent tree is the idempotent no-op.
func removeRunTreeFallback(ctx context.Context, path string) error {
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
