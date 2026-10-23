//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// jailTmpDirName is the directory under the state root that holds every
// jail's /tmp, one entry per subnet index.
//
// A jail's /tmp is a directory on the executor's disk rather than a tmpfs
// because of where a tmpfs keeps its files: in gVisor's memory, charged to
// the jail's cgroup, where the kernel cannot reclaim them. A jail that fills
// a tmpfs /tmp to its memory ceiling leaves the kernel nothing to kill that
// frees memory but the runtime itself, and the jail ends. On disk the same
// files are page cache, which the kernel writes back and reclaims instead.
const jailTmpDirName = "jail-tmp"

// jailTmpMode closes a jail's /tmp, and the directory holding them all, to
// every host identity but the owner. runsc mounts a jail's directory as root,
// so nothing else on the host needs to reach either.
const jailTmpMode = 0o700

// prepareJailTmp gives the network at subnetIdx an empty /tmp for its jails
// and returns its host path. It runs when the network is built, so whatever
// the jails of an earlier network on the same index left there is removed
// first.
func prepareJailTmp(ctx context.Context, subnetIdx uint8) (string, error) {
	return jailTmpDir(ctx, subnetIdx, true)
}

// jailTmp returns the host path of the /tmp a jail launched into the network
// at subnetIdx mounts. It is not emptied: a jail relaunched after the one
// before it died finds that jail's files, the output of whatever it was
// running among them. It is made again if it is missing, and its owner and
// mode are set again whatever they are, because the jail owns it and may have
// changed them; refusing the launch instead would let a command in the jail
// end its run.
func jailTmp(ctx context.Context, subnetIdx uint8) (string, error) {
	return jailTmpDir(ctx, subnetIdx, false)
}

// jailTmpDir makes the /tmp of the network at subnetIdx the jail's own,
// emptying it first when empty is set.
//
// Everything here is derived from the subnet index the caller's network was
// validated against, never taken from the caller, and every operation is
// relative to a descriptor on the base: the state root belongs to the
// orchestrator, so a path re-resolved after a check could be swapped under the
// broker. The path it returns is resolved once more by runsc when it mounts
// it, the same residual the run tree's mount carries.
func jailTmpDir(ctx context.Context, subnetIdx uint8, empty bool) (string, error) {
	baseFd, base, err := openJailTmpBase(true)
	if err != nil {
		return "", fmt.Errorf("sandbox: jail /tmp: %w", err)
	}
	defer closeFd(baseFd)

	name := jailTmpEntryName(subnetIdx)
	dir := filepath.Join(base, name)
	if empty {
		if err := removeJailTmpEntry(ctx, baseFd, dir, name); err != nil {
			return "", fmt.Errorf("sandbox: empty jail /tmp %s: %w", dir, err)
		}
	}
	if err := unix.Mkdirat(baseFd, name, jailTmpMode); err != nil && (empty || !errors.Is(err, unix.EEXIST)) {
		return "", fmt.Errorf("sandbox: create jail /tmp %s: %w", dir, err)
	}
	fd, err := openat2AnchoredChildDir(baseFd, name)
	if err != nil {
		return "", fmt.Errorf("sandbox: open jail /tmp %s: %w", dir, err)
	}
	defer closeFd(fd)
	if err := unix.Fchown(fd, JailUID(subnetIdx), JailGID(subnetIdx)); err != nil {
		return "", fmt.Errorf("sandbox: hand jail /tmp %s to its jail: %w", dir, err)
	}
	// Mkdirat's mode passed through this process's umask.
	if err := unix.Fchmod(fd, jailTmpMode); err != nil {
		return "", fmt.Errorf("sandbox: chmod jail /tmp %s: %w", dir, err)
	}
	return dir, nil
}

// removeJailTmp removes the /tmp of the jail at subnetIdx, with everything in
// it. A /tmp that is already gone, or a base never made, is nothing to remove.
func removeJailTmp(ctx context.Context, subnetIdx uint8) error {
	baseFd, base, err := openJailTmpBase(false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: jail /tmp: %w", err)
	}
	defer closeFd(baseFd)
	name := jailTmpEntryName(subnetIdx)
	if err := removeJailTmpEntry(ctx, baseFd, filepath.Join(base, name), name); err != nil {
		return fmt.Errorf("sandbox: remove jail /tmp %s: %w", filepath.Join(base, name), err)
	}
	return nil
}

// reapOrphanJailTmp removes every jail's /tmp a previous process left behind.
// It runs once at boot, before any jail of this process exists, so every
// entry belongs to a jail that is gone; and only the broker writes in the
// base, so there is nothing in it to leave.
func reapOrphanJailTmp(ctx context.Context) {
	baseFd, base, err := openJailTmpBase(false)
	if errors.Is(err, unix.ENOENT) {
		return
	}
	if err != nil {
		sandboxLog.Warn("orphan jail /tmp directories not reaped", "error", err)
		return
	}
	defer closeFd(baseFd)
	names, err := readDirNames(baseFd)
	if err != nil {
		sandboxLog.Warn("orphan jail /tmp directories not reaped", "path", base, "error", err)
		return
	}
	for _, name := range names {
		if err := removeJailTmpEntry(ctx, baseFd, filepath.Join(base, name), name); err != nil {
			sandboxLog.Warn("orphan jail /tmp not removed", "path", filepath.Join(base, name), "error", err)
		}
	}
}

func jailTmpEntryName(subnetIdx uint8) string {
	return strconv.Itoa(int(subnetIdx))
}

// openJailTmpBase opens the directory every jail's /tmp lives in and returns
// a descriptor on it with its path, creating it first when create is set.
//
// The state root is resolved to its real path and opened refusing any
// symlink, and the base itself is opened O_NOFOLLOW. The base must then be
// what this process makes it: owned by this process, closed to everyone
// else, and on a filesystem that keeps its files on disk. A base the
// orchestrator made or replaced fails the ownership check rather than being
// trusted with a jail's files.
func openJailTmpBase(create bool) (fd int, base string, err error) {
	root, err := paths.StateRootErr()
	if err != nil {
		return -1, "", fmt.Errorf("resolve state root: %w", err)
	}
	realRoot, err := realPath(root)
	if err != nil {
		return -1, "", err
	}
	base = filepath.Join(realRoot, jailTmpDirName)
	rootFd, err := openDirRefusingSymlinks(realRoot)
	if err != nil {
		return -1, base, fmt.Errorf("open state root %s: %w", realRoot, err)
	}
	defer closeFd(rootFd)
	created := false
	if create {
		err := unix.Mkdirat(rootFd, jailTmpDirName, jailTmpMode)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, base, fmt.Errorf("create %s: %w", base, err)
		}
		created = err == nil
	}
	fd, err = openat2AnchoredChildDir(rootFd, jailTmpDirName)
	if err != nil {
		return -1, base, fmt.Errorf("open %s: %w", base, err)
	}
	// Mkdirat's mode passed through this process's umask.
	if created {
		if err := unix.Fchmod(fd, jailTmpMode); err != nil {
			closeFd(fd)
			return -1, base, fmt.Errorf("chmod %s: %w", base, err)
		}
	}
	if err := checkJailTmpBase(fd); err != nil {
		closeFd(fd)
		return -1, base, fmt.Errorf("%s: %w", base, err)
	}
	return fd, base, nil
}

// checkJailTmpBase holds the base to what openJailTmpBase makes it.
func checkJailTmpBase(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("fstat: %w", err)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("owned by uid %d, not this process (%d); only the broker makes it", st.Uid, os.Geteuid())
	}
	if perm := st.Mode & 0o7777; perm != jailTmpMode {
		return fmt.Errorf("mode %#o, want %#o", perm, jailTmpMode)
	}
	var sfs unix.Statfs_t
	if err := unix.Fstatfs(fd, &sfs); err != nil {
		return fmt.Errorf("fstatfs: %w", err)
	}
	if fs := memoryBackedFS(&sfs); fs != "" {
		return fmt.Errorf("on %s, which holds a jail's /tmp in memory; the state root must be on disk", fs)
	}
	return nil
}

// removeJailTmpEntry removes name under baseFd, whatever it is, without
// following a symlink out of the base. path names it in errors.
func removeJailTmpEntry(ctx context.Context, baseFd int, path, name string) error {
	fd, err := openat2AnchoredChildDir(baseFd, name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		// A symlink or a file at the name: it goes as itself.
		if err := unix.Unlinkat(baseFd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unlinkat %s: %w", path, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	err = removeDirChildren(ctx, fd, path)
	closeFd(fd)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(baseFd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unlinkat(rmdir) %s: %w", path, err)
	}
	return nil
}

// openDirRefusingSymlinks opens dir with every component of it refused if it
// is a symlink, where the kernel can refuse that (openat2), and plainly
// otherwise; dir is already a resolved real path either way.
func openDirRefusingSymlinks(dir string) (int, error) {
	fd, err := openat2Syscall(unix.AT_FDCWD, dir, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		return unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	}
	return fd, err
}

// memoryBackedFS names the filesystem st describes when it keeps its files in
// memory, and returns "" otherwise. The magic numbers are 32-bit, which is
// all the comparison reads on every architecture.
func memoryBackedFS(st *unix.Statfs_t) string {
	switch uint32(st.Type) {
	case unix.TMPFS_MAGIC:
		return "tmpfs"
	case unix.RAMFS_MAGIC:
		return "ramfs"
	}
	return ""
}

// CheckStateRootOnDisk refuses a state root on a filesystem that keeps its
// files in memory. Every jail's /tmp lives under it, so on such a filesystem a
// jail that fills its /tmp fills its memory ceiling with files the kernel
// cannot reclaim, and the jail ends. Boot calls it, so the deployment fails
// at once with the fix named rather than at the first full /tmp; the broker
// checks the base again at every launch.
func CheckStateRootOnDisk() error {
	root, err := paths.StateRootErr()
	if err != nil {
		return fmt.Errorf("resolve state root: %w", err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return fmt.Errorf("statfs state root %s: %w", root, err)
	}
	if fs := memoryBackedFS(&st); fs != "" {
		return fmt.Errorf("the state root %s is on %s: every jail's /tmp lives under it, and on a filesystem held in memory a jail that fills its /tmp ends. Point TF_STATE_ROOT at a directory on disk", root, fs)
	}
	return nil
}
