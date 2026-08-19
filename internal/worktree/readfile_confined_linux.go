//go:build linux

package worktree

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

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
	f, err := openFileConfined(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func openFileConfined(root, rel string) (*os.File, error) {
	dirfd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirfd) }()

	fd, err := unix.Openat2(dirfd, rel, &unix.OpenHow{
		Flags:   uint64(os.O_RDONLY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH,
	})
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			// openat2 predates this kernel (< 5.6) or is blocked by seccomp. The
			// sandbox requires a modern kernel, so this is not expected in the
			// context the confinement protects — but do NOT fall open to a raw
			// os.ReadFile, which would silently restore the exact symlink-follow
			// this exists to close. Fall back to an EvalSymlinks-based containment
			// check (mirrors internal/sandbox/runtree_linux.go's pre-5.6 path) and
			// warn once so a silent degrade is diagnosable.
			noteReadConfinedOpenat2Unsupported(err)
			return openFileConfinedFallback(root, rel)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// readFileConfinedFallback is the pre-openat2 path: resolve every symlink in the
// full path, reject the read if the resolved target escapes root, and read the
// RESOLVED path (not the symlink path) so a component swapped after the check
// still can't redirect the read outside root. A narrower TOCTOU than a raw read,
// and it only ever runs on a kernel too old to sandbox on in the first place.
func readFileConfinedFallback(root, rel string) ([]byte, error) {
	f, err := openFileConfinedFallback(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func openFileConfinedFallback(root, rel string) (*os.File, error) {
	full := filepath.Join(root, rel)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, err
	}
	relResolved, err := filepath.Rel(root, resolved)
	if err != nil || relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("worktree: confined read of %q resolves outside the run root (%q); refusing", rel, resolved)
	}
	return os.Open(resolved)
}

// readConfinedOpenat2Unsupported latches the "this kernel can't do openat2"
// finding process-wide so the warning fires once, not per capture.
var readConfinedOpenat2Unsupported sync.Once

func noteReadConfinedOpenat2Unsupported(err error) {
	readConfinedOpenat2Unsupported.Do(func() {
		worktreeLog.Warn("confined transcript read: openat2(RESOLVE_BENEATH) unsupported by this kernel (need Linux 5.6+); falling back to EvalSymlinks-based containment", "err", err)
	})
}
