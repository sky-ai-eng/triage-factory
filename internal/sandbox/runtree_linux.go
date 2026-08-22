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

func captureRunDelta(ctx context.Context, worktree, stagingDir, sessionID string) ([]byte, error) {
	return defaultOps.CaptureRunDelta(ctx, worktree, stagingDir, sessionID)
}

// runTreeHandedOff stats path and reports whether the sandbox identity owns it
// while this process does not — the post-chown state. Deliberately a plain
// Lstat, not a privileged op: reading a mode/owner needs no capability, and the
// answer only ever suppresses a doomed write.
func runTreeHandedOff(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == WorktreeUID && os.Geteuid() != WorktreeUID
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

// runTreeGroupGID is the GROUP a run tree is chowned to at the ownership
// hand-off, and the write-access channel that lets the orchestrator
// materialize checkouts into a run root it has otherwise handed to the
// sandbox agent. It defaults to WorktreeGID — the agent's own gid, the
// historical target, correct for the in-process/dev case where the tree
// creator and the agent are one identity. In the brokered split the broker
// sets it to the ORCHESTRATOR's gid via SetRunTreeGroupGID. That choice is
// load-bearing: the run root becomes group-writable by the orchestrator
// WITHOUT granting WorktreeGID, the group the per-run credential sidecar
// also carries — granting WorktreeGID would let any run's sidecar reach
// every other run's tree over the shared executor filesystem. The
// orchestrator's gid is carried by no sandboxed principal (not the agent,
// not any sidecar), so a run tree stays reachable by exactly the owning
// agent and the trusted orchestrator, and by nothing else.
var runTreeGroupGID = WorktreeGID

// SetRunTreeGroupGID registers the orchestrator's gid as the run-tree group
// target. Called once by the cap-broker subcommand from --orchestrator-gid
// at boot, before serving; never in-process (dev/same-uid keeps the
// WorktreeGID default, where owner and agent coincide anyway).
func SetRunTreeGroupGID(gid int) { runTreeGroupGID = gid }

// runTreeDirMode is the mode the run-tree directory scaffold — the run root
// and the owner/repo intermediates a mid-run `workspace add` mints — carries:
// rwx for the owning agent (owner) AND the orchestrator (group
// runTreeGroupGID), and nothing for anyone else. The group-write bit is
// exactly what lets the orchestrator create checkouts inside a run root it has
// handed to the agent.
//
// The scaffold is MINTED at this mode (mkdirRunTreeScaffold) and re-asserted
// at the ownership hand-off. Both, not either: the hand-off's recursive form
// preserves the modes it finds, so a scaffold directory that reached it at an
// ordinary mkdir's 0755 would be frozen orchestrator-unwritable, and the
// hand-off in turn is what moves a freshly minted level to the sandbox
// identity so the agent can traverse its own checkout.
//
// Only the scaffold directories are set to this — a checkout's own tree keeps
// whatever modes git wrote (executable bits and so on); being reachable
// THROUGH the scaffold is all the orchestrator needs there.
const runTreeDirMode = 0o770

// chmodRunTreeDir sets a pinned run-tree directory to runTreeDirMode, using
// the SAME pinned fd the chown acted on so a rename racing the walk can't
// redirect it: a real fd is chmod'd directly; an O_PATH fd (the fallback the
// walk takes when a dir can't be opened readable) can't be Fchmod'd, so it
// goes through that fd's /proc magic symlink, which still resolves to exactly
// the pinned inode.
func chmodRunTreeDir(fd int, isPathFd bool) error {
	if !isPathFd {
		return unix.Fchmod(fd, uint32(runTreeDirMode))
	}
	return unix.Fchmodat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), uint32(runTreeDirMode), 0)
}

// mkdirRunTreeScaffold walks rel a component at a time from a directory fd
// pinned on root, creating each level at runTreeDirMode. Every step is
// fd-relative (Mkdirat/Openat off the level above, O_NOFOLLOW refusing a
// symlink entry outright), so no component is ever re-resolved as a path
// string that a swap racing the walk could redirect.
//
// This is an ORDINARY, capability-less operation — it creates directories the
// calling process then owns, and asserts a mode on those. It needs no broker
// hop precisely because it never touches an inode it did not just mint; see
// ensureScaffoldMode for what happens when it meets one that it did.
func mkdirRunTreeScaffold(root, rel string) error {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("sandbox: mkdir run tree scaffold: subpath %q escapes root %q", rel, root)
	}

	dirFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	ownDirFd := true
	defer func() {
		if ownDirFd {
			closeFd(dirFd)
		}
	}()

	display := root
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		display = filepath.Join(display, seg)
		if err := unix.Mkdirat(dirFd, seg, uint32(runTreeDirMode)); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("mkdirat %s: %w", display, err)
		}
		fd, err := openat2AnchoredChildDir(dirFd, seg)
		if err != nil {
			return fmt.Errorf("open %s: %w", display, err)
		}
		if err := ensureScaffoldMode(fd); err != nil {
			closeFd(fd)
			return fmt.Errorf("fchmod %s: %w", display, err)
		}
		if ownDirFd {
			closeFd(dirFd)
		}
		dirFd, ownDirFd = fd, true
	}
	return nil
}

// ensureScaffoldMode asserts runTreeDirMode on a pinned scaffold directory.
// Mkdirat's mode argument is masked by the process umask — the usual 022
// clears exactly the group-write bit the scaffold exists to carry — so the
// mode has to be set rather than merely requested at creation.
//
// A directory this process does not own is left alone. That is the level a
// previous run already handed to the sandbox identity, which a capability-less
// orchestrator cannot chmod at all; failing here would turn a tree that is
// merely already correct into a dead one. If such a level is genuinely
// unwritable, the Mkdirat one component below is where it surfaces — named
// for the level that is actually wrong, rather than for this one.
func ensureScaffoldMode(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&^uint32(unix.S_IFMT) == uint32(runTreeDirMode) {
		return nil
	}
	err := unix.Fchmod(fd, uint32(runTreeDirMode))
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		return nil
	}
	return err
}

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
// chownRunTreeFallback / removeRunTreeFallback (pre-5.6-kernel path) and the
// capture child's unprivileged worktree input. Capture staging is instead
// opened and validated through pinned descriptors in capture_linux.go.
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
// fd. Used only for the top-level ChownRunTree/RemoveRunTree argument
// itself (root/path's immediate open under the openat2-resolved parent) —
// recursive descent below goes through openEntryPinned/listableDirFd
// instead, which pin an entry's identity before any ownership check runs
// against it. O_NOFOLLOW refuses a symlink entry outright (ELOOP) rather
// than following it, and O_DIRECTORY refuses anything that isn't actually
// a directory (ENOTDIR) — root/path is contractually always a directory.
func openat2AnchoredChildDir(dirFd int, name string) (int, error) {
	return unix.Openat(dirFd, name, unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
}

// openEntryPinned resolves dirFd/name EXACTLY ONCE and returns an fd
// pinned to that specific inode, plus its stat info read from that SAME
// fd — so a caller's owner check (against st) and its subsequent
// chown/descend (against fd) always act on the identical inode, immune to
// a rename/swap of `name` in dirFd's directory after this call returns.
// This is what closes the residual TOCTOU a naive
// "Fstatat(name) then Fchownat(name)" pair still has: two separate
// by-name resolutions of the same entry can race a concurrent rename
// between them; one resolution followed by fd-only operations cannot.
//
// The first attempt is a plain, non-O_PATH open (cheap, and already
// usable directly for getdents if it's a directory). That fails ELOOP for
// a symlink entry (O_NOFOLLOW refuses to open one normally) and can fail
// for other reasons on unusual entry types a plain open() can't handle at
// all (e.g. ENXIO opening a UNIX domain socket file) — either way, the
// retry with O_PATH succeeds uniformly regardless of entry type (a
// symlink pinned as itself, never followed; any other type pinned without
// needing to actually "open" it), matching what the fallback's Lchown
// could always do by path.
func openEntryPinned(dirFd int, name string) (fd int, st unix.Stat_t, isPathFd bool, err error) {
	fd, err = unix.Openat(dirFd, name, unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		fd, err = unix.Openat(dirFd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, st, false, err
		}
		isPathFd = true
	}
	if err = unix.Fstat(fd, &st); err != nil {
		closeFd(fd)
		return -1, st, false, err
	}
	return fd, st, isPathFd, nil
}

// chownPinnedEntry chowns the inode fd refers to via Fchownat's
// AT_EMPTY_PATH form ("operate on fd itself, resolve no name"), which
// works whether fd is a plain or an O_PATH descriptor. Because it never
// re-resolves `name`, it always chowns the exact inode openEntryPinned
// pinned — whatever `name` has since been renamed to, or replaced with.
func chownPinnedEntry(fd int) error {
	return unix.Fchownat(fd, "", WorktreeUID, runTreeGroupGID, unix.AT_EMPTY_PATH)
}

// listableDirFd returns an fd usable with getdents for the directory
// openEntryPinned identified. A plain (non-O_PATH) fd already qualifies
// and is returned as-is (distinct=false: it IS fd, the caller must not
// separately close it). An O_PATH fd can't be used for getdents, so this
// "upgrades" it into a real fd for the SAME inode via the kernel's
// /proc/self/fd magic-symlink resolution — which resolves against this
// process's own open-file table, not a second lookup of the entry's name
// in its parent directory, so a rename racing the original open can't
// redirect it (distinct=true: the caller owns and must close the
// returned fd separately from fd).
func listableDirFd(fd int, isPathFd bool) (listFd int, distinct bool, err error) {
	if !isPathFd {
		return fd, false, nil
	}
	listFd, err = unix.Openat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false, err
	}
	return listFd, true, nil
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
// prior check) if any component of its parent is a symlink. Every op
// after that resolves an entry's name exactly once (openEntryPinned) and
// operates on the resulting fd from then on — the owner check
// (allowedRunTreeOwner against Fstat) and the chown (Fchownat's
// AT_EMPTY_PATH form) both act on that one pinned fd, so a rename racing
// between "check" and "chown" cannot substitute a different inode than
// the one that was checked. A symlink entry is pinned as itself (never
// followed) and chowned as itself. An entry planted with a foreign owner
// fails the whole operation closed, exactly like the fallback's
// lchownRunTreeEntry.
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
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := chownPinnedEntry(rootFd); err != nil {
			return fmt.Errorf("fchownat %s: %w", root, err)
		}
		// The root is the run root's scaffold directory — set it group-writable
		// (runTreeDirMode) so the orchestrator can materialize checkouts into it
		// later; its children keep whatever modes they were created with.
		// rootFd came from openat2AnchoredChildDir (a real O_DIRECTORY fd), so a
		// direct Fchmod applies.
		if err := chmodRunTreeDir(rootFd, false); err != nil {
			return fmt.Errorf("fchmod %s: %w", root, err)
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

		fd, st, isPathFd, err := openEntryPinned(dirFd, seg)
		if err != nil {
			opErr = fmt.Errorf("open %s: %w", display, err)
			break
		}
		if !allowedRunTreeOwner(st.Uid) {
			closeFd(fd)
			opErr = fmt.Errorf("%s is owned by uid %d, not a run-tree owner", display, st.Uid)
			break
		}
		if err := chownPinnedEntry(fd); err != nil {
			closeFd(fd)
			opErr = fmt.Errorf("fchownat %s: %w", display, err)
			break
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			closeFd(fd)
			opErr = fmt.Errorf("chown run tree: subpath intermediate %q is not a directory", display)
			break
		}
		// A scaffold intermediate (owner/, owner/repo/): make it group-writable
		// so a later same-owner `workspace add` can mint sibling checkouts under
		// it. Same pinned fd as the chown, so the chmod can't be redirected.
		if err := chmodRunTreeDir(fd, isPathFd); err != nil {
			closeFd(fd)
			opErr = fmt.Errorf("fchmod %s: %w", display, err)
			break
		}
		listFd, distinct, err := listableDirFd(fd, isPathFd)
		if err != nil {
			closeFd(fd)
			opErr = fmt.Errorf("open %s for listing: %w", display, err)
			break
		}
		if distinct {
			closeFd(fd)
		}
		if ownDirFd {
			closeFd(dirFd)
		}
		dirFd = listFd
		ownDirFd = true
	}
	if ownDirFd {
		closeFd(dirFd)
	}
	return opErr
}

// chownEntryRecursive pins dirFd/name (openEntryPinned), checks and
// chowns it via that one pinned fd, and — if it is itself a directory —
// obtains a listable fd for it (listableDirFd) and recurses over
// everything inside.
func chownEntryRecursive(ctx context.Context, dirFd int, name, displayPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fd, st, isPathFd, err := openEntryPinned(dirFd, name)
	if err != nil {
		return fmt.Errorf("open %s: %w", displayPath, err)
	}
	if !allowedRunTreeOwner(st.Uid) {
		closeFd(fd)
		return fmt.Errorf("%s is owned by uid %d, not a run-tree owner", displayPath, st.Uid)
	}
	if err := chownPinnedEntry(fd); err != nil {
		closeFd(fd)
		return fmt.Errorf("fchownat %s: %w", displayPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		closeFd(fd)
		return nil
	}
	listFd, distinct, err := listableDirFd(fd, isPathFd)
	if err != nil {
		closeFd(fd)
		return fmt.Errorf("open %s for listing: %w", displayPath, err)
	}
	if distinct {
		closeFd(fd)
	}
	defer closeFd(listFd)
	return chownDirChildren(ctx, listFd, displayPath)
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
// fd-relative — the contents go regardless of what uids the run left
// inside (agent-created files arrive via the privileged gofer and can
// carry modes the orchestrator could never unlink through, which is
// exactly why this op lives on the privileged side), matching the
// fallback's no-interior-ownership-check behavior exactly. A missing
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

// removeDirChildren empties dirFd's directory contents: each entry is
// pinned once (openEntryPinned) to learn its type from the SAME fd a
// directory entry is then recursed into (listableDirFd) — no ownership
// check here, matching removeRunTreeFallback's os.RemoveAll, which never
// inspected interior uids either. The terminal Unlinkat/Unlinkat(..
// AT_REMOVEDIR) is necessarily by name (Linux has no unlink-by-fd
// primitive); by the time it runs, a directory has already been fully
// emptied via the pinned fd, so a rename racing the entry's name at that
// last step can only make the unlink itself fail (e.g. ENOTEMPTY/ENOTDIR
// against whatever now occupies the name) rather than silently remove
// the wrong tree's contents.
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
		fd, st, isPathFd, err := openEntryPinned(dirFd, name)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("open %s: %w", entryPath, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			closeFd(fd)
			if err := unix.Unlinkat(dirFd, name, 0); err != nil {
				return fmt.Errorf("unlinkat %s: %w", entryPath, err)
			}
			continue
		}
		listFd, distinct, err := listableDirFd(fd, isPathFd)
		if err != nil {
			closeFd(fd)
			return fmt.Errorf("open %s for listing: %w", entryPath, err)
		}
		if distinct {
			closeFd(fd)
		}
		err = removeDirChildren(ctx, listFd, entryPath)
		closeFd(listFd)
		if err != nil {
			return err
		}
		if err := unix.Unlinkat(dirFd, name, unix.AT_REMOVEDIR); err != nil {
			return fmt.Errorf("unlinkat(rmdir) %s: %w", entryPath, err)
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
