//go:build linux

package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/internal/capinfo"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// captureCommand builds the child command that runs the git-delta capture. A
// package var so tests can point it at a helper process instead of re-execing
// the real binary. Production re-invokes this same binary's internal
// `snapshot-capture` subcommand.
var captureCommand = func(ctx context.Context, wtPath string) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	return exec.CommandContext(ctx, self, "snapshot-capture", wtPath), nil
}

// captureIsolated runs CaptureWorkspaceGit for wtPath inside a
// dropped-privilege, network-isolated child and returns the decoded delta.
//
// The child drops to the sandbox uid/gid (matching the run-root chown) and runs
// in a fresh, empty network namespace. So: any clean/smudge/textconv/diff
// filter a hostile .git/config could trigger executes only as the agent's own
// uid with no network — not as the host root — which is the whole point; and
// the reader's uid matching the tree owner means git's own capture commands
// (rev-parse/bundle/diff) no longer fail dubious-ownership.
//
// The network namespace is defense in depth on top of the uid drop, not
// the primary boundary — that's the uid drop itself (see below) — so it
// is applied only when this process actually holds CAP_SYS_ADMIN
// (creating a network namespace needs it). An orchestrator running with
// its capabilities dropped at exec (the default; see
// docker/entrypoint.sh) has none, so it skips CLONE_NEWNET entirely
// rather than failing the whole capture on a privileged syscall it can
// never make. Routing this through the cap-broker instead, so the
// network isolation applies unconditionally again, is tracked
// separately.
//
// Dropping to the sandbox uid is SUFFICIENT for the whole capture only because
// a multi-mode delegated run's worktree is always a SELF-CONTAINED clone: its
// .git is a real directory fully inside the run root, so chownWorktreeForSandbox
// covers config, objects, and refs together. A linked `git worktree` (the
// local-mode / curator layout) keeps its objects + config in a separate bare
// cache that is never chowned and stays root-owned — dropping to uid 10000 there
// would trade dubious-ownership for EACCES on the bare. Multi mode never uses
// that layout (the sandbox can't see the shared bare), a property pinned by
// worktree.TestCreateForPR_SelfContainedClone_MultiMode; if that ever changes,
// this capture must resolve the commondir's ownership too. The child receives a
// deliberately minimal environment: never the TF process env (which carries DB
// and service credentials), plus config overrides that neuter the two
// attribute-free exec vectors (core.fsmonitor, diff.external) as defense in
// depth on top of the uid drop.
func captureIsolated(ctx context.Context, wtPath string) (*worktree.GitDelta, error) {
	cmd, err := captureCommand(ctx, wtPath)
	if err != nil {
		return nil, err
	}
	cmd.Dir = "/" // a neutral cwd; the child is passed the worktree path explicitly
	cmd.Env = captureChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(sandbox.WorktreeUID),
			Gid: uint32(sandbox.WorktreeGID),
			// Empty Groups (NoSetGroups stays false) forces setgroups(0), shedding
			// the parent root process's supplementary groups — otherwise the child
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

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 || string(out) == "null" {
		return nil, nil // non-git run root: no delta
	}
	var delta worktree.GitDelta
	if err := json.Unmarshal(out, &delta); err != nil {
		return nil, fmt.Errorf("isolated capture: decode delta: %w", err)
	}
	return &delta, nil
}

// captureChildEnv is the minimal environment for the capture child. It
// deliberately does NOT inherit the TF process env — that holds DB passwords and
// service tokens the child (running attacker-influenceable git) must never see.
// It carries only what git needs to run locally.
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
