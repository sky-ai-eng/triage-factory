//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
	"golang.org/x/sys/unix"
)

// captureCommand builds the child command that runs the git-delta capture. A
// package var so tests can point it at a helper process instead of re-execing
// the real binary. Production re-invokes this same binary's internal
// `snapshot-capture` subcommand — os.Executable() resolves to the same
// triagefactory binary; this capture runs inside the cap-broker, which is
// itself a re-exec of that binary.
var captureCommand = func(ctx context.Context, wtPath, stagingDir, sessionID string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	argv := []string{"snapshot-capture", wtPath, stagingDir}
	if sessionID != "" {
		argv = append(argv, sessionID)
	}
	return exec.CommandContext(ctx, self, argv...), nil
}

// CaptureManifestMaxBytes bounds the capture child's small JSON manifest. The
// bundle, patch, and transcript never cross stdout, so a manifest approaching
// this ceiling is malformed rather than a legitimate large workspace.
var CaptureManifestMaxBytes int64 = 1 * 1024 * 1024

// captureStderrTailBytes bounds how much of the capture child's stderr a
// caller gets back. Diagnostics only — never run data — so a small fixed
// tail is enough; the stdout stream is what needed the real cap above.
const captureStderrTailBytes = 64 * 1024

// ReadCaptureManifest reads the bounded JSON control message shared by the
// fallback and brokered paths.
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
// diagnostic tail for the capture child's stderr, which is never run data
// but is unbounded in principle (a hostile .git/config could make git chatty
// on stderr), so it gets the same "bound it, don't trust it" treatment as
// the stdout stream, just with a much smaller ceiling.
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

// CaptureRunDeltaTo runs CaptureWorkspaceGit for worktree inside a
// dropped-privilege, network-isolated child exactly as the buffered
// CaptureRunDelta below does, but streams the child's stdout directly into
// the caller-supplied file instead of buffering it in this process.
//
// stdout MUST be a real *os.File assigned directly to cmd.Stdout, never
// wrapped in another io.Writer: os/exec special-cases *os.File (dup2
// straight into the child at exec time, no copy in this process); any other
// io.Writer forces os/exec to fall back to an os.Pipe() plus a copy
// goroutine RIGHT HERE. Keeping the passed-through fd preserves the broker's
// payload-blind posture even though stdout now carries only the small manifest;
// the large members go to stagingDir.
//
// The child drops to the sandbox uid/gid (matching the run tree's ownership
// hand-off) and runs in a fresh, empty network namespace. So: any
// clean/smudge/textconv/diff filter a hostile .git/config could trigger
// executes only as the agent's own uid with no network — not as this
// (privileged) process — which is the whole point; and the reader's uid
// matching the tree owner means git's own capture commands
// (rev-parse/bundle/diff) no longer fail dubious-ownership.
//
// The network namespace is defense in depth on top of the uid drop, not the
// primary boundary — that's the uid drop itself (see below) — so it is
// applied only when this process actually holds CAP_SYS_ADMIN (creating a
// network namespace needs it). This method runs in the cap-broker, which
// holds CAP_SYS_ADMIN in the container deployment. The gate survives for
// the unprivileged bare-metal dev case, where it skips CLONE_NEWNET rather
// than failing the whole capture on a syscall it can never make.
//
// Dropping to the sandbox uid is SUFFICIENT for the whole capture only
// because a multi-mode delegated run's worktree is always a SELF-CONTAINED
// clone: its .git is a real directory fully inside the run root, so the run
// tree's ownership hand-off covers config, objects, and refs together. A
// linked `git worktree` (the local-mode / curator layout) keeps its objects
// + config in a separate bare cache that is never re-owned — dropping to
// uid 10000 there would trade dubious-ownership for EACCES on the bare.
// Multi mode never uses that layout (the sandbox can't see the shared
// bare), a property pinned by worktree.TestCreateForPR_SelfContainedClone_MultiMode;
// if that ever changes, this capture must resolve the commondir's ownership
// too. The child receives a deliberately minimal environment: never this
// process's env (the orchestrator's carries DB and service credentials; the
// broker's carries its flags), plus the config overrides CaptureChildEnv
// documents — defense in depth on top of the uid drop, never the boundary.
//
// Returns a bounded tail of the child's stderr (diagnostics, never run
// data) regardless of outcome, plus the run's error if any.
func CaptureRunDeltaTo(ctx context.Context, worktree, stagingDir, sessionID string, stdout *os.File) (stderrTail string, err error) {
	// stdout is this function's to close, whichever way it returns: on a
	// validation or captureCommand failure below nobody else will ever hold
	// a reference to it, and once the child is spawned its own inherited
	// dup is what keeps the pipe/socket alive — this process's copy must
	// drop regardless so a reader blocked on EOF is not held open by us.
	defer func() { _ = stdout.Close() }()

	if _, err := validateRunTreeRoot("capture run delta", worktree); err != nil {
		return "", err
	}
	outputs, err := openCaptureStaging(stagingDir)
	if err != nil {
		return "", err
	}
	defer func() {
		for _, f := range outputs {
			_ = f.Close()
		}
	}()

	cmd, err := captureCommand(ctx, worktree, stagingDir, sessionID)
	if err != nil {
		return "", err
	}
	cmd.Dir = "/" // a neutral cwd; the child is passed the worktree path explicitly
	cmd.Env = CaptureChildEnv()
	cmd.ExtraFiles = outputs
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(WorktreeUID),
			Gid: uint32(WorktreeGID),
			// Empty Groups (NoSetGroups stays false) forces setgroups(0), shedding
			// the parent process's supplementary groups — otherwise the child
			// keeps group 0 (root) et al. and retains group-readable/writable host
			// access it must not have.
			Groups: []uint32{},
		},
	}
	if hasSysAdmin() {
		// Empty network namespace: capture is local-only. Skipped entirely
		// when this process has no CAP_SYS_ADMIN (see the doc above) —
		// creating a netns without it fails the clone, not just the
		// isolation.
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNET
	}

	stderr := &tailBuffer{max: captureStderrTailBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("isolated capture: %w", err)
	}
	return stderr.String(), nil
}

func openCaptureStaging(dir string) ([]*os.File, error) {
	if err := validateRunTreeShape("capture staging", dir); err != nil {
		return nil, err
	}
	dirFD, wantUID, err := openCaptureStagingDir(dir)
	if err != nil {
		return nil, err
	}
	defer closeFd(dirFD)
	return openCaptureStagingMembers(dirFD, wantUID)
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

func openCaptureStagingMembers(dirFD, wantUID int) ([]*os.File, error) {
	var files []*os.File
	for _, name := range []string{CaptureBundleFile, CapturePatchFile, CaptureTranscriptFile} {
		f, err := openCaptureStagingMember(dirFD, name, wantUID)
		if err != nil {
			for _, opened := range files {
				_ = opened.Close()
			}
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
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

// CaptureRunDelta is the unprivileged, in-process fallback used only when
// no broker exists (unprivileged bare-metal dev): a thin buffered wrapper
// over CaptureRunDeltaTo. It pipes the manifest into itself, reading
// concurrently with the child's Run and enforcing the same small control-plane
// ceiling the brokered path applies via ReadCaptureManifest.
func (hostOps) CaptureRunDelta(ctx context.Context, worktree, stagingDir, sessionID string) ([]byte, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("isolated capture: create pipe: %w", err)
	}

	type readResult struct {
		buf []byte
		err error
	}
	readDone := make(chan readResult, 1)
	go func() {
		buf, err := ReadCaptureManifest(pr)
		_ = pr.Close()
		readDone <- readResult{buf, err}
	}()

	stderrTail, runErr := CaptureRunDeltaTo(ctx, worktree, stagingDir, sessionID, pw)
	res := <-readDone
	if runErr != nil {
		if stderrTail != "" {
			return nil, fmt.Errorf("%w: %s", runErr, stderrTail)
		}
		return nil, runErr
	}
	if res.err != nil {
		return nil, fmt.Errorf("isolated capture: %w", res.err)
	}
	return res.buf, nil
}

// hasSysAdmin reports whether this process's own effective capability set
// includes CAP_SYS_ADMIN — needed to create the capture child's network
// namespace. A package var (like captureCommand above) so a test can
// substitute it without needing to actually run with (or without) the
// capability. Read fresh each call rather than cached: cheap (one small
// /proc/self/status read) and correct even if a future caller somehow
// changes this process's capability set between calls, which caching
// would silently miss.
var hasSysAdmin = func() bool {
	names, err := capinfo.Effective()
	if err != nil {
		return false
	}
	for _, n := range names {
		if n == "cap_sys_admin" {
			return true
		}
	}
	return false
}
