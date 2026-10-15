//go:build linux

package sandbox

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// CaptureManifestMaxBytes bounds the capture jail's small JSON manifest. The
// bundles, patches and scratch files never cross stdout, so a manifest
// approaching this ceiling is malformed rather than a legitimate large
// workspace.
var CaptureManifestMaxBytes int64 = 1 * 1024 * 1024

// ReadCaptureManifest reads the capture jail's bounded JSON control message.
func ReadCaptureManifest(r io.Reader) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, CaptureManifestMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read capture manifest: %w", err)
	}
	if int64(len(buf)) > CaptureManifestMaxBytes {
		return nil, fmt.Errorf("capture manifest exceeds %d byte cap", CaptureManifestMaxBytes)
	}
	return buf, nil
}

// tailBuffer retains only the last max bytes ever written to it — a bounded
// diagnostic tail for a jail's stderr, which is never run data but is
// unbounded in principle (a hostile .git/config could make git chatty on
// stderr), so it gets the same "bound it, don't trust it" treatment as the
// manifest, just with a much smaller ceiling.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

// Write never lets the transient allocation exceed max, even for a single
// huge p: trimming AFTER an unconditional append (the naive approach) would
// briefly hold len(t.buf)+len(p) bytes, defeating the point of a hard cap
// against a hostile child that dumps a large chunk to stderr in one write.
func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if len(t.buf)+n > t.max {
		t.truncated = true
	}
	if n >= t.max {
		// p alone already fills (or overflows) the cap: keep only its own
		// tail — a fresh copy, so this doesn't hold a reference into p's
		// (possibly much larger) backing array — discarding everything
		// buffered so far without ever appending past max.
		t.buf = append([]byte(nil), p[n-t.max:]...)
		return n, nil
	}
	// Make room for p first, then append — len(t.buf) never exceeds max at
	// any point, including mid-call.
	if keep := t.max - n; len(t.buf) > keep {
		t.buf = t.buf[len(t.buf)-keep:]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	s := strings.TrimSpace(string(t.buf))
	if t.truncated {
		return "...(truncated)... " + s
	}
	return s
}

// captureIdentity is the uid/gid the capture jail runs as: the jail identity
// that owns worktree, read from the stat validateRunTreeRoot already took.
// Read from the tree rather than named by the caller because only this side
// can make a jail uid own anything, so the owner is a statement of which
// engagement wrote the tree that no caller can forge.
func captureIdentity(worktree string, info os.FileInfo) (uid, gid int, err error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("sandbox: capture run delta: no stat for %s", worktree)
	}
	idx, ok := jailSubnetIdx(int(st.Uid))
	if !ok {
		return 0, 0, fmt.Errorf("sandbox: capture run delta: %s is owned by uid %d, which is not a jail identity", worktree, st.Uid)
	}
	return JailUID(idx), JailGID(idx), nil
}

// openCaptureStaging opens the capture's one output file: the stream the jail
// writes its members to. The orchestrator prepared it; this validates and
// truncates it, and returns a descriptor the jail is handed.
func openCaptureStaging(dir string) (*os.File, error) {
	if err := validateRunTreeShape("capture staging", dir); err != nil {
		return nil, err
	}
	dirFD, wantUID, err := openCaptureStagingDir(dir)
	if err != nil {
		return nil, err
	}
	defer closeFd(dirFD)
	return openCaptureStagingMember(dirFD, CaptureStreamFile, wantUID)
}

// openCaptureStagingDir resolves every component through pinned directory
// descriptors. The untrusted orchestrator can rename or replace any path it
// owns while the broker is serving the request; once this returns, later
// member opens stay on the exact directory inode resolved here.
func openCaptureStagingDir(path string) (fd, wantUID int, err error) {
	fd, err = unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, 0, fmt.Errorf("sandbox: capture staging: open root anchor: %w", err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, openErr := unix.Openat(fd, component, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeFd(fd)
		if openErr != nil {
			return -1, 0, fmt.Errorf("sandbox: capture staging: open anchored directory %s: %w", path, openErr)
		}
		fd = next
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		closeFd(fd)
		return -1, 0, fmt.Errorf("sandbox: capture staging: fstat directory %s: %w", path, err)
	}
	wantUID = os.Getuid()
	if runTreeOwnerExtraUID >= 0 {
		wantUID = runTreeOwnerExtraUID
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o7777 != 0o700 {
		closeFd(fd)
		return -1, 0, fmt.Errorf("sandbox: capture staging: %s must be a 0700 directory", path)
	}
	if int(st.Uid) != wantUID {
		closeFd(fd)
		return -1, 0, fmt.Errorf("sandbox: capture staging: %s is not owned by orchestrator uid %d", path, wantUID)
	}
	return fd, wantUID, nil
}

// openCaptureStagingMember validates a harmless O_PATH handle before gaining
// write access. Reopening /proc/self/fd pins the write handle to that same
// inode; no attacker-controlled path is resolved between validation and
// truncation.
func openCaptureStagingMember(dirFD int, name string, wantUID int) (*os.File, error) {
	pathFD, err := pinCaptureStagingMember(dirFD, name)
	if err != nil {
		return nil, err
	}
	defer closeFd(pathFD)
	return openPinnedCaptureStagingMember(pathFD, name, wantUID)
}

func pinCaptureStagingMember(dirFD int, name string) (int, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("sandbox: open capture staging member %s: %w", name, err)
	}
	return fd, nil
}

func openPinnedCaptureStagingMember(pathFD int, name string, wantUID int) (*os.File, error) {
	var pinned unix.Stat_t
	if err := unix.Fstat(pathFD, &pinned); err != nil {
		return nil, fmt.Errorf("sandbox: fstat capture staging member %s: %w", name, err)
	}
	if !validCaptureStagingMemberStat(&pinned, wantUID) {
		return nil, fmt.Errorf("sandbox: capture staging: %s must be a single-link regular 0600 file owned by orchestrator uid %d", name, wantUID)
	}

	fd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(pathFD), unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("sandbox: reopen pinned capture staging member %s: %w", name, err)
	}
	var writable unix.Stat_t
	if err := unix.Fstat(fd, &writable); err != nil {
		closeFd(fd)
		return nil, fmt.Errorf("sandbox: fstat writable capture staging member %s: %w", name, err)
	}
	if writable.Dev != pinned.Dev || writable.Ino != pinned.Ino || !validCaptureStagingMemberStat(&writable, wantUID) {
		closeFd(fd)
		return nil, fmt.Errorf("sandbox: capture staging member %s changed while opening or no longer satisfies its boundary", name)
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		closeFd(fd)
		return nil, fmt.Errorf("sandbox: truncate capture staging member %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		closeFd(fd)
		return nil, fmt.Errorf("sandbox: wrap capture staging member %s", name)
	}
	return f, nil
}

func validCaptureStagingMemberStat(st *unix.Stat_t, wantUID int) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&0o7777 == 0o600 && int(st.Uid) == wantUID && st.Nlink == 1
}
