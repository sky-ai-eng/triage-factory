//go:build linux

package worktree

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// readFileConfined reads root/rel, refusing to traverse any symlink or ".." that
// would resolve OUTSIDE root. It is the isolation boundary for reading an
// agent-written file (the Claude session transcript) inside the snapshot-capture
// child: that child runs on the host filesystem as the shared agent uid
// (sandbox.WorktreeUID, the same for every run), so a plain read would follow an
// agent-planted symlink out of the run root into another run's tree — owned by
// that same uid — or any host file it can read. RESOLVE_BENEATH makes the kernel
// fail the open (EXDEV) the instant resolution would leave root, atomically —
// with none of the TOCTOU window a stat-then-read would leave a hostile agent.
func readFileConfined(root, rel string) ([]byte, error) {
	dirfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)

	fd, err := unix.Openat2(dirfd, rel, &unix.OpenHow{
		Flags:   uint64(os.O_RDONLY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH,
	})
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			// openat2 predates this kernel (< 5.6). That only happens outside the
			// sandbox — the gVisor capture child requires a modern kernel — where
			// there is no cross-uid boundary to protect, so a plain read is safe.
			return os.ReadFile(filepath.Join(root, rel))
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	return io.ReadAll(f)
}
