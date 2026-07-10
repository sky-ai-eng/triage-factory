//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
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
// triagefactory binary whether this runs in-process (TF_PRIVSEP=0) or inside
// the cap-broker (which is itself a re-exec of that binary).
var captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	return exec.CommandContext(ctx, self, "snapshot-capture", wtPath), nil
}

// CaptureRunDelta runs CaptureWorkspaceGit for worktree inside a
// dropped-privilege, network-isolated child and returns its raw JSON stdout.
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
// network namespace needs it). In the default deployment this method runs in
// the cap-broker, which always does; the in-process TF_PRIVSEP=0 path runs
// as root and does too. The gate survives for the unprivileged bare-metal
// dev case, where it skips CLONE_NEWNET rather than failing the whole
// capture on a syscall it can never make.
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
func (hostOps) CaptureRunDelta(ctx context.Context, worktree string) ([]byte, error) {
	if _, err := validateRunTreeRoot("capture run delta", worktree); err != nil {
		return nil, err
	}

	cmd, err := captureCommand(ctx, worktree)
	if err != nil {
		return nil, err
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

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("isolated capture: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
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
