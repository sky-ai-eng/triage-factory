//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
)

// captureCommand builds the child command that runs the git-delta capture. A
// package var so tests can point it at a helper process instead of re-execing
// the real binary. Production re-invokes this same binary's internal
// `snapshot-capture` subcommand — os.Executable() resolves to the same
// triagefactory binary; this capture runs inside the cap-broker, which is
// itself a re-exec of that binary.
var captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	return exec.CommandContext(ctx, self, "snapshot-capture", wtPath), nil
}

// CaptureMaxBytes bounds how many raw bytes of a capture child's stdout
// either capture path — the buffered in-process fallback below, or the
// cap-broker's streamed RPC (cmd/capbroker) — will hold before giving up.
// This is the true raw ceiling: the frame cap this replaces applied to the
// JSON response AFTER base64 inflation, an effective ~384 MiB raw limit, so
// this is never tighter than what shipped before. A var (not a const) only
// so tests can shrink it rather than pushing real hundreds-of-MiB payloads.
var CaptureMaxBytes int64 = 512 * 1024 * 1024

// captureStderrTailBytes bounds how much of the capture child's stderr a
// caller gets back. Diagnostics only — never run data — so a small fixed
// tail is enough; the stdout stream is what needed the real cap above.
const captureStderrTailBytes = 64 * 1024

// ReadCapturedDelta reads r up to CaptureMaxBytes, erroring instead of
// silently truncating if more arrives — the shared ceiling both capture
// paths enforce on the same raw byte stream (decision: "one ceiling"),
// applied by the reader in each case: hostOps.CaptureRunDelta's own pipe
// below, and the orchestrator-side IPCClient reading the brokered stream.
func ReadCapturedDelta(r io.Reader) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, CaptureMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read captured delta: %w", err)
	}
	if int64(len(buf)) > CaptureMaxBytes {
		return nil, fmt.Errorf("capture stream exceeds %d byte cap", CaptureMaxBytes)
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

func (t *tailBuffer) Write(p []byte) (int, error) {
	if len(t.buf)+len(p) > t.max {
		t.truncated = true
	}
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
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
// goroutine RIGHT HERE, silently reintroducing every byte of the delta into
// this process's memory and defeating the reason this function exists. This
// is what lets the cap-broker call it with the orchestrator's
// passed-through socket fd and never read a byte of the delta itself.
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
// broker's carries its flags), plus config overrides that neuter the two
// attribute-free exec vectors (core.fsmonitor, diff.external) as defense in
// depth on top of the uid drop.
//
// Returns a bounded tail of the child's stderr (diagnostics, never run
// data) regardless of outcome, plus the run's error if any.
func CaptureRunDeltaTo(ctx context.Context, worktree string, stdout *os.File) (stderrTail string, err error) {
	// stdout is this function's to close, whichever way it returns: on a
	// validation or captureCommand failure below nobody else will ever hold
	// a reference to it, and once the child is spawned its own inherited
	// dup is what keeps the pipe/socket alive — this process's copy must
	// drop regardless so a reader blocked on EOF is not held open by us.
	defer func() { _ = stdout.Close() }()

	if _, err := validateRunTreeRoot("capture run delta", worktree); err != nil {
		return "", err
	}

	cmd, err := captureCommand(ctx, worktree)
	if err != nil {
		return "", err
	}
	cmd.Dir = "/" // a neutral cwd; the child is passed the worktree path explicitly
	cmd.Env = captureChildEnv()
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

// CaptureRunDelta is the unprivileged, in-process fallback used only when
// no broker exists (unprivileged bare-metal dev): a thin buffered wrapper
// over CaptureRunDeltaTo. It pipes the streaming variant's stdout into
// itself, reading concurrently with the child's Run (a pipe's kernel buffer
// is a few tens of KiB — anything larger would deadlock a child that writes
// before anyone drains it) and enforcing the same CaptureMaxBytes ceiling
// the brokered path's client-side reader applies, via ReadCapturedDelta.
func (hostOps) CaptureRunDelta(ctx context.Context, worktree string) ([]byte, error) {
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
		buf, err := ReadCapturedDelta(pr)
		_ = pr.Close()
		readDone <- readResult{buf, err}
	}()

	stderrTail, runErr := CaptureRunDeltaTo(ctx, worktree, pw)
	res := <-readDone
	if runErr != nil {
		return nil, fmt.Errorf("%w: %s", runErr, stderrTail)
	}
	if res.err != nil {
		return nil, fmt.Errorf("isolated capture: %w", res.err)
	}
	return res.buf, nil
}

// captureChildEnv is the minimal environment for the capture child. It
// deliberately does NOT inherit this process's env — the orchestrator's holds
// DB passwords and service tokens the child (running attacker-influenceable
// git) must never see. It carries only what git needs to run locally.
//
// It also refuses to read any user/global/system git config: GIT_CONFIG_GLOBAL
// and GIT_CONFIG_SYSTEM point at /dev/null so git ignores ~/.gitconfig, the XDG
// global config, and /etc/gitconfig, and HOME is a non-existent path so nothing
// resolves through it either. Without this, HOME pointing at a shared writable
// directory (/tmp) would let anyone plant /tmp/.gitconfig — a filter, an
// include.path — and make the capture attacker-influenceable and
// non-deterministic across runs. Only the run root's own repo config is read.
// On top of that, config overrides neuter the two attribute-free code-exec keys
// (core.fsmonitor, diff.external) as defense in depth; the uid drop, not these,
// is the actual boundary.
func captureChildEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), // locate the git binary
		"HOME=/nonexistent",         // no user config from a shared/writable HOME
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=diff.external", "GIT_CONFIG_VALUE_1=",
	}
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
