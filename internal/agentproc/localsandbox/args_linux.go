//go:build linux

package localsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Args returns the bubblewrap argv the agent command is prefixed with,
// ending in the "--" that closes bwrap's own options. The caller appends the
// command it wanted to run — Args never sees or names it.
//
// The plan is ordered, because bwrap applies operations in argv order and the
// order IS the policy: mask first, bind back second, grants last. Reading it
// top to bottom is reading what the agent can reach.
//
// Pure: no filesystem access, no environment. Whether the paths it names
// exist is Preflight's question, asked separately so a missing run-scoped
// grant fails with its own name rather than as a bwrap mount error.
func Args(spec Spec, host Host) ([]string, error) {
	if spec.RunRoot == "" {
		return nil, errors.New("localsandbox: Spec.RunRoot is required")
	}
	if host.Home == "" {
		return nil, errors.New("localsandbox: Host.Home is required")
	}
	if host.Cwd == "" {
		return nil, errors.New("localsandbox: Host.Cwd is required")
	}
	for _, p := range append(append([]string{spec.RunRoot, host.Home, host.Cwd}, spec.AddDirs...), spec.ReadOnly...) {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("localsandbox: %q is not an absolute path (the mount plan binds every path at its real location)", p)
		}
	}

	// The whole host, read-only, plus the two pseudo-filesystems that cannot
	// simply be carried over: a minimal /dev (no host disks, no /dev/kmsg)
	// and a fresh /proc. No --unshare-net (the run's git proxy, gh injector
	// and LLM endpoint are loopback) and no --unshare-pid (the caller's
	// Setpgid + kill-process-group teardown reaches the agent's children
	// only while they share this pid namespace; --die-with-parent below is
	// what covers the case --unshare-pid would have).
	args := []string{
		Binary,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}

	// Phase 1 — every mask, before any bind-back. Four trees are replaced
	// with empty tmpfs:
	//
	//
	//   the operator's home    dotfiles, ssh keys, every other project on the
	//                          machine — none of which a run needs, since its
	//                          identity, credentials and remotes all arrive
	//                          through env-scoped config and a loopback proxy
	//   the TF state root      the SQLite database, the bare clone cache, the
	//                          curator projects, every OTHER conversation's
	//                          gh-channel dir
	//   the temp dir           every concurrent run's worktree — the thing
	//                          this package exists for, since before it two
	//                          runs' trees were flat siblings under one uid
	//                          and `ls` plus `cat` read either. /tmp is masked
	//                          alongside it whenever TMPDIR points elsewhere,
	//                          so the machine's shared temp goes too
	//   /run                   which takes /run/user/<uid> with it — the
	//                          session D-Bus and Secret Service sockets — so
	//                          the OS keychain is simply unreachable
	//
	// Sorted, which puts a parent ahead of any child of it (a prefix sorts
	// first), and emitted as a group so no later mask can shadow an earlier
	// bind-back. Without that grouping the plan would depend on how the four
	// happen to nest on a given host.
	masks := dedupe([]string{host.Home, host.StateRoot, "/tmp", host.TempDir, "/run"})
	slices.Sort(masks)
	for _, m := range masks {
		args = append(args, "--tmpfs", m)
	}

	// Phase 2 — everything that comes back through them. -try on the
	// host-owned directories, which may legitimately not exist yet on a fresh
	// install (no hooks written, no gh fetched, no ~/.claude); strict on the
	// run's own, which Preflight has already established are there.
	args = appendBindTry(args, "--bind-try", host.ClaudeDir)
	args = appendBindTry(args, "--ro-bind-try", host.HooksDir, host.GHBinDir)
	args = appendBindTry(args, "--bind-try", spec.GHChannelDir)
	args = append(args, "--bind", spec.RunRoot, spec.RunRoot)
	if spec.AgentHostSocket != "" {
		args = append(args, "--bind", spec.AgentHostSocket, AgentHostSocketDest)
	}
	// Host paths the run resolved rather than configured — the node
	// interpreter, this binary, the SDK install. -try because a race that
	// removes one between resolution and spawn should surface as the command
	// failing to exec, not as an unrelated mount error.
	args = appendBindTry(args, "--ro-bind-try", spec.ReadOnly...)

	// Phase 3 — grants last, so a directory the operator explicitly handed
	// this run wins over anything above it. rw because that is what the grant
	// means.
	for _, dir := range dedupe(spec.AddDirs) {
		args = append(args, "--bind", dir, dir)
	}

	args = append(args,
		"--die-with-parent",
		"--chdir", host.Cwd,
		"--",
	)
	return args, nil
}

// appendBindTry appends one -try bind per non-empty path, source and
// destination identical, skipping duplicates. Identical source and
// destination is the invariant the whole plan rests on: nothing is relocated,
// so every path the agent is told about — its cwd, its run root, an --add-dir
// grant — means the same thing inside the namespace as outside it.
func appendBindTry(args []string, flag string, paths ...string) []string {
	for _, p := range dedupe(paths) {
		args = append(args, flag, p, p)
	}
	return args
}

// dedupe returns the cleaned, non-empty paths in order, without repeats.
func dedupe(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		c := filepath.Clean(p)
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// Preflight checks the parts of the plan that must exist on disk before bwrap
// reads them, and creates the one directory the plan would rather not find
// missing.
//
// Strictness is split by who owns the path. A run-scoped path — the run root,
// an --add-dir grant, the agenthost socket — is named by this run and must be
// there: a grant that silently vanishes leaves the agent reading an empty
// directory as if it were the answer, which is worse than a failed run. A
// host-owned path — the hooks dir, the pinned gh dir, ~/.claude — may
// legitimately not exist yet on a fresh install, so Args binds those with
// -try and this function only ensures ~/.claude, whose absence would send the
// run's transcript to a tmpfs that disappears with the namespace.
func Preflight(spec Spec, host Host) error {
	if host.ClaudeDir != "" {
		if err := os.MkdirAll(host.ClaudeDir, 0o700); err != nil {
			return fmt.Errorf("localsandbox: create %s: %w", host.ClaudeDir, err)
		}
	}
	if err := mustExist(spec.RunRoot, "run root"); err != nil {
		return err
	}
	if spec.AgentHostSocket != "" {
		if err := mustExist(spec.AgentHostSocket, "agenthost socket"); err != nil {
			return err
		}
	}
	for _, dir := range dedupe(spec.AddDirs) {
		if err := mustExist(dir, "--add-dir grant"); err != nil {
			return err
		}
	}
	return nil
}

func mustExist(path, what string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("localsandbox: %s %s is not on disk: %w", what, path, err)
	}
	return nil
}

// probeTimeout bounds the boot probe. Both probe steps are a fork+exec of a
// program that does nothing; a host where that takes seconds has a problem
// the operator needs told about either way.
const probeTimeout = 10 * time.Second

// Probe reports whether bubblewrap can actually build a namespace on this
// host. Boot calls it when the sandbox is on and refuses to start if it
// fails — never a silent downgrade to an unsandboxed run.
//
// It is two steps because they fail for different reasons and the operator
// fixes them differently. `--version` answers "is bubblewrap installed".
// The smoke run answers "may this user create a namespace", which on Ubuntu
// 23.10+ is a separate question entirely: unprivileged user namespaces are
// restricted by AppArmor, and it is the distro bubblewrap package's own
// profile that permits them — so a hand-built bwrap, or a TF running inside
// a container that blocks nested userns, passes the first step and fails the
// second.
func Probe() error {
	bin, err := exec.LookPath(Binary)
	if err != nil {
		return fmt.Errorf("%s is not on PATH: %w", Binary, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%s --version failed: %w (%s)", bin, err, firstLine(out))
	}
	// The smoke run uses the same argv SHAPE a real spawn does — including
	// the "--" terminator — so a bubblewrap too old to understand one of
	// them is caught here rather than on the first delegated run.
	smoke := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--die-with-parent", "--", trueBinary()}
	if out, err := exec.CommandContext(ctx, bin, smoke...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s could not create a namespace: %w (%s)", bin, err, firstLine(out))
	}
	return nil
}

// trueBinary resolves the do-nothing program the smoke run executes inside
// the namespace. /bin/true is the fallback for a PATH that cannot find it;
// a host with neither fails the smoke run, which is the correct answer for
// an environment that cannot run a trivial command.
func trueBinary() string {
	if p, err := exec.LookPath("true"); err == nil {
		return p
	}
	return "/bin/true"
}

func firstLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "no output"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
